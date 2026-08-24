// Package rest implements the generated OpenAPI ServerInterface for Quack's REST surface.
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Workspace identity for fs/git tools and per-chat jail. NOT the ADK session identity (see sessionUser).
const userID = "local"

// ADK session identity for webhook-dispatched runs (fallback for older chats without SessionUser).
const githubSessionUser = "github"

// Resolves ADK session identity for a chat's turns/events. A GitHub-dispatched chat's identity varies per chat (#512).
func (h *Handler) sessionUser(ctx context.Context, chatID string) string {
	return h.store.SessionUserForChat(ctx, chatID)
}

const titleInstruction = "Generate a concise chat title (3–6 words, no punctuation, no quotes). Return only the title."

// Backstop that stops a wedged run leaking a goroutine (24h covers the longest overnight DAG).
const runTimeout = 24 * time.Hour

// Handler implements schema.ServerInterface backed by the store + orchestrator.
type Handler struct {
	store        *store.Store
	orch         *orchestrator.Orchestrator
	titler       model.LLM
	jail         *workspace.Jail         // per-chat workspace tree cleanup on delete; nil ⇒ no workspace configured
	hub          *stream.Hub             // fans a chat's run to extra subscribers (other devices); also the cancel-run registry (#468)
	eventLog     *runlog.EventLog        // durably persists the run stream, backing replay across restarts
	ledgerStore  ledger.LedgerStore      // replay ledger backend; nil ⇒ recording disabled, GetChatRecording 404s
	quackVersion string                  // build stamp, stamped into a recording bundle's manifest.json
	taskMem      *memory.Store           // repo:/role: buckets; nil ⇒ task memory disabled
	userMem      *memory.Store           // user: buckets; nil ⇒ user memory disabled
	artifacts    *store.TurnAwareService // durable attachment bytes; nil ⇒ multipart attachments are dropped (see saveAttachment)
	extensions   []schema.ExtensionInfo  // enabled SDK extensions for GET /api/v1/extensions; nil ⇒ empty list
}

// NewHandler builds a REST handler. jail/hub/ledgerStore/taskMem/userMem/artifacts/extensions may be nil; hub defaults to a private hub.
func NewHandler(s *store.Store, o *orchestrator.Orchestrator, titler model.LLM, jail *workspace.Jail, hub *stream.Hub, ledgerStore ledger.LedgerStore, quackVersion string, taskMem, userMem *memory.Store, artifacts *store.TurnAwareService, extensions []schema.ExtensionInfo) *Handler {
	if hub == nil {
		hub = stream.NewHub()
	}
	return &Handler{store: s, orch: o, titler: titler, jail: jail, hub: hub, eventLog: runlog.NewEventLog(s), ledgerStore: ledgerStore, quackVersion: quackVersion, taskMem: taskMem, userMem: userMem, artifacts: artifacts, extensions: extensions}
}

func (h *Handler) generateTitle(ctx context.Context, chatID, firstMessage string) string {
	if h.titler == nil {
		return ""
	}
	// ChatID-only Coords so this call is filed under the chat, not "unscoped" (#617).
	ctx = ledger.WithCoords(ctx, ledger.Coords{ChatID: chatID})
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "/no_think " + firstMessage}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: titleInstruction}}},
		},
	}
	var out strings.Builder
	var candidates, total int32
	for resp, err := range h.titler.GenerateContent(ctx, req, false) {
		if err != nil {
			slog.Warn("title generation failed; using empty title", "component", "title", "err", err)
			return ""
		}
		if resp.UsageMetadata != nil {
			candidates = resp.UsageMetadata.CandidatesTokenCount
			total = resp.UsageMetadata.TotalTokenCount
		}
		if resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if !p.Thought && p.Text != "" {
				out.WriteString(p.Text)
			}
		}
	}
	// Strip leaked thinking blocks that /no_think doesn't always suppress.
	title := stream.StripThinking(out.String())
	slog.Info("title generated", "component", "title", "title", title, "candidates", candidates, "total", total)
	return title
}

func (h *Handler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// errInvalidStatus is chatsScopeFor's client-error sentinel: an explicitly
// empty or unrecognized status selection is a 400, never silently
// "everything" and never a 500.
var errInvalidStatus = errors.New("invalid status")

// chatsScopeFor reconciles status (the current API, repeatable/multi-select)
// with the deprecated show_archived bool: status wins when given; otherwise
// show_archived=true maps to {active,archived} and false/absent to {active},
// its pre-#809 behavior. Order in the status list is irrelevant - it's
// collapsed into two flags, so ?status=active&status=archived and the
// reverse order produce the identical scope (and page token).
func chatsScopeFor(params schema.ListChatsParams) (store.ChatsScope, error) {
	if params.Status != nil {
		if len(*params.Status) == 0 {
			return store.ChatsScope{}, fmt.Errorf("%w: status must not be empty", errInvalidStatus)
		}
		var scope store.ChatsScope
		for _, st := range *params.Status {
			switch st {
			case schema.Active:
				scope.Active = true
			case schema.Archived:
				scope.Archived = true
			default:
				return store.ChatsScope{}, fmt.Errorf("%w: %q", errInvalidStatus, st)
			}
		}
		return scope, nil
	}
	if params.ShowArchived != nil && *params.ShowArchived {
		return store.ChatsScope{Active: true, Archived: true}, nil
	}
	return store.ChatsScope{Active: true}, nil
}

// ListChats is a single table read (#738: status is a stamp on the chat row - see
// store.StampRunOutcome - plus cheap in-memory hub/queue checks, not a per-chat DB read).
// It's also a conditional GET: an unchanged page costs a 304 with no body, so the SPA's
// 5s poll is cheap on the wire when nothing changed (still no TTL - every poll reaches
// this handler and revalidates against the live rows). The ETag is hashed from the
// marshaled page body, which embeds NextPageToken, so it varies with page token and
// limit as well as content - a stale ETag from a different page never reads as a match.
func (h *Handler) ListChats(w http.ResponseWriter, r *http.Request, params schema.ListChatsParams) {
	limit := 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	pageToken := ""
	if params.PageToken != nil {
		pageToken = *params.PageToken
	}
	scope, err := chatsScopeFor(params)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	chats, next, err := h.store.ListChats(r.Context(), limit, pageToken, scope)
	if err != nil {
		if errors.Is(err, store.ErrInvalidPageToken) {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		httpError(w, http.StatusInternalServerError, err)
		return
	}

	ids := make([]string, len(chats))
	for i, c := range chats {
		ids[i] = c.ID
	}
	totals, err := h.store.ChatsUsageTotals(r.Context(), ids)
	if err != nil {
		// Token totals are a nice-to-have on the list; don't fail the page over it.
		slog.Warn("list chats: usage totals failed", "component", "rest", "err", err)
		totals = map[string]int64{}
	}

	out := schema.ChatList{Data: make([]schema.ChatSummary, len(chats))}
	for i, c := range chats {
		out.Data[i] = h.toSummary(c, totals[c.ID])
	}
	if next != "" {
		out.NextPageToken = &next
	}
	body, err := json.Marshal(out)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	etag := weakETag(body)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// weakETag hashes an already-serialized body - cheap, and content (not metadata) is what
// the client cares about matching (#738 test 5).
func weakETag(body []byte) string {
	sum := fnv.New64a()
	_, _ = sum.Write(body)
	return fmt.Sprintf(`W/"%x"`, sum.Sum64())
}

func (h *Handler) CreateChat(w http.ResponseWriter, r *http.Request) {
	var body schema.CreateChatBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	sp := ""
	if body.SystemPrompt != nil {
		sp = *body.SystemPrompt
	}
	c, err := h.store.CreateChat(r.Context(), sp)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	// A brand-new chat has no turns/nodes yet - 0 tokens, no query needed.
	writeJSON(w, http.StatusOK, h.toSummary(*c, 0))
}

func (h *Handler) GetChat(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	c, err := h.store.GetChat(r.Context(), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if c == nil {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	turns, err := h.store.GetTurnsWithContent(r.Context(), orchestrator.AppName, store.SessionUserFor(*c), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	status, pendingQuestion := h.chatStatus(r.Context(), chatID, turns)
	usage, err := h.store.GetChatUsage(r.Context(), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	detail := schema.ChatDetail{
		Id:              c.ID,
		Title:           strPtr(c.Title),
		SystemPrompt:    c.SystemPrompt,
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
		Status:          status,
		PendingQuestion: pendingQuestion,
		GithubUrl:       strPtr(c.GithubURL),
		GithubRepo:      strPtr(c.GithubRepo),
		GithubState:     detailStateVal(c.GithubState),
		Archived:        boolPtr(c.Archived),
		Origin:          chatOrigin(c.Origin),
		Turns:           make([]schema.Turn, 0, len(turns)),
		Usage:           usageAggregateToSchema(usage),
	}
	if usage.TotalTokens > 0 {
		detail.TotalTokens = intPtr(int(usage.TotalTokens))
	}
	for _, tc := range turns {
		detail.Turns = append(detail.Turns, buildTurn(tc))
	}
	writeJSON(w, http.StatusOK, detail)
}

// terminalNodeID returns the plan's terminal node - the one no other node
// depends on. Its output IS the turn's answer, mirroring the frontend's
// terminalNodeId/liveDagFinalText.
func terminalNodeID(plan stream.DagPlanData) string {
	hasSuccessor := make(map[string]bool, len(plan.Edges))
	for _, e := range plan.Edges {
		hasSuccessor[e.From] = true
	}
	for _, n := range plan.Nodes {
		if !hasSuccessor[n.ID] {
			return n.ID
		}
	}
	return ""
}

// terminalNodeOutput returns the terminal node's full vetted output, or ""
// when the plan has no terminal node with output yet.
func terminalNodeOutput(plan stream.DagPlanData, nodes []store.DagNode) string {
	id := terminalNodeID(plan)
	if id == "" {
		return ""
	}
	for _, n := range nodes {
		if n.NodeID == id {
			return strings.TrimSpace(n.Output)
		}
	}
	return ""
}

// usageAggregateToSchema always populates every field (unlike Turn.usage,
// which is sparse) - ChatDetail.usage is the chat-wide total, meaningfully
// zero rather than absent.
func usageAggregateToSchema(u store.UsageAggregate) schema.Usage {
	return schema.Usage{
		InputTokens:     intPtr(int(u.InputTokens)),
		OutputTokens:    intPtr(int(u.OutputTokens)),
		ReasoningTokens: intPtr(int(u.ReasoningTokens)),
		CachedTokens:    intPtr(int(u.CachedTokens)),
		TotalTokens:     intPtr(int(u.TotalTokens)),
	}
}

func (h *Handler) GetResponse(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, responseID schema.ResponseID) {
	tc, err := h.store.GetTurnWithContent(r.Context(), orchestrator.AppName, h.sessionUser(r.Context(), chatID), chatID, responseID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if tc == nil {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, buildTurn(*tc))
}

// ListExtensions lists every enabled SDK extension for the SPA nav -
// name-only unless the module implements the SDK's optional UI descriptor.
func (h *Handler) ListExtensions(w http.ResponseWriter, r *http.Request) {
	out := h.extensions
	if out == nil {
		out = []schema.ExtensionInfo{}
	}
	writeJSON(w, http.StatusOK, out)
}

// Lists sessions the replay ledger has entries for (backs `quack recording list`).
func (h *Handler) ListRecordings(w http.ResponseWriter, r *http.Request) {
	if h.ledgerStore == nil {
		errMsg(w, http.StatusNotFound, "recording is not enabled")
		return
	}
	refs, err := h.ledgerStore.List(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	out := schema.RecordingList{Data: make([]schema.RecordingSummary, len(refs))}
	for i, ref := range refs {
		out.Data[i] = schema.RecordingSummary{
			ChatId:     ref.ID,
			SizeBytes:  ref.Size,
			ModifiedAt: ref.ModTime,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// Streams the chat's replay-ledger recording as a ZIP. ReadStream check first ensures a clean 404.
func (h *Handler) GetChatRecording(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	if h.ledgerStore == nil {
		errMsg(w, http.StatusNotFound, "recording is not enabled")
		return
	}
	entries, err := h.ledgerStore.ReadStream(r.Context(), chatID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			errMsg(w, http.StatusNotFound, "no recording for this chat")
			return
		}
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	defer entries.Close()

	w.Header().Set("Content-Type", "application/zip")
	// mime.FormatMediaType quotes the filename; chatID is caller-supplied so never reaches the header verbatim.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": chatID + ".zip"}))
	if err := ledger.AssembleBundle(r.Context(), h.ledgerStore, chatID, h.quackVersion, otelobs.GenAISemConvVersion, entries, w); err != nil {
		// Headers (and possibly partial body) already sent; log only.
		slog.Warn("recording bundle write failed mid-stream", "component", "rest", "chat", chatID, "err", err)
	}
}

// Partial update to a chat's mutable metadata (title or archived flag). At least one must
// be present. Only updating archived does not touch UpdatedAt so the list stays recency-ordered.
func (h *Handler) UpdateChat(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	var body schema.UpdateChatBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errMsg(w, http.StatusBadRequest, "invalid body")
		return
	}

	hasTitle := body.Title != nil && strings.TrimSpace(*body.Title) != ""
	hasArchived := body.Archived != nil
	if !hasTitle && !hasArchived {
		errMsg(w, http.StatusBadRequest, "at least one of title or archived must be provided")
		return
	}

	c, err := h.store.GetChat(r.Context(), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if c == nil {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}

	if hasTitle {
		title := strings.TrimSpace(*body.Title)
		if err := h.store.UpdateTitle(r.Context(), chatID, title); err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		c.Title = title
	}
	if hasArchived {
		if err := h.store.ArchiveChat(r.Context(), chatID, *body.Archived); err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		c.Archived = *body.Archived
		// A run still waiting behind max_active_runs must not fire later against
		// an archived chat - cancel it the same id-guarded way a user Stop does.
		// A run already executing (Queued false) is left alone.
		if *body.Archived && h.orch.Queued(chatID) && c.ActiveTurnID != "" {
			h.hub.CancelResponse(chatID, c.ActiveTurnID)
		}
	}

	writeJSON(w, http.StatusOK, h.toSummary(*c, h.chatTotalTokens(r.Context(), chatID)))
}

func (h *Handler) DeleteChat(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	// Kill any live run on this chat FIRST so we don't drop the row while it still executes (#468).
	h.hub.CancelRun(chatID)
	if err := h.store.DeleteChat(r.Context(), chatID); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	// Best-effort cleanup of the per-chat workspace tree. RemoveChatScope sanitizes chatID so it can't escape.
	if h.jail != nil {
		if err := h.jail.RemoveChatScope(userID, chatID); err != nil {
			slog.Warn("per-chat workspace cleanup failed; chat deleted anyway",
				"component", "rest", "chat", chatID, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Starts a run and streams it as SSE. Accepts JSON or multipart/form-data (with optional file attachments).
func (h *Handler) SendChatMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	if !h.requireChat(w, r, chatID) {
		return
	}
	if h.hub.Draining() {
		errMsg(w, http.StatusServiceUnavailable, "server is shutting down; try again shortly")
		return
	}
	var body schema.SendMessageBody
	var attachments []*genai.Part

	// Generate a stable turn ID before the run so the DAG plan can reference it, the cancel
	// endpoint can name it, and (below) an uploaded attachment can be stamped with it.
	turnID := uuid.NewString()

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			errMsg(w, http.StatusBadRequest, "invalid multipart body")
			return
		}
		body.Content = r.FormValue("content")
		userID := h.sessionUser(r.Context(), chatID)
		i := 0
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				f, err := fh.Open()
				if err != nil {
					continue
				}
				data, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					continue
				}
				mimeType := fh.Header.Get("Content-Type")
				if mimeType == "" {
					mimeType = "application/octet-stream"
				}
				name := fh.Filename
				if name == "" {
					name = fmt.Sprintf("attachment-%d", i)
				}
				i++
				ref, err := h.saveAttachment(r.Context(), userID, chatID, turnID, name, data, mimeType)
				if err != nil {
					slog.Warn("attachment save failed; dropping this file", "component", "rest", "chat", chatID, "name", name, "err", err)
					continue
				}
				attachments = append(attachments, ref)
			}
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == "" {
			errMsg(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	if body.Content == "" {
		errMsg(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sse, ok := newSSEWriter(w)
	if !ok {
		errMsg(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	go func() {
		if err := h.store.SaveTurn(context.Background(), chatID, turnID); err != nil {
			slog.Warn("save turn failed", "component", "rest", "chat", chatID, "err", err)
		}
	}()

	// Subscribe BEFORE the run starts publishing so nothing is missed.
	h.hub.Reset(chatID)
	replay, live, unsubscribe, _ := h.hub.Subscribe(chatID)
	defer unsubscribe()

	h.startRun(chatID, turnID, body.Content, attachments)

	// From here this handler is only a viewer - it cannot stall or kill the run.
	streamHub(r.Context(), sse, replay, live, 0)
}

// saveAttachment durably stores one uploaded file's bytes and returns a
// lightweight reference part (internal/artifactref) - never the bytes - to
// carry through plans and session events instead.
func (h *Handler) saveAttachment(ctx context.Context, userID, chatID, turnID, name string, data []byte, mimeType string) (*genai.Part, error) {
	if h.artifacts == nil {
		return nil, fmt.Errorf("no artifact service configured")
	}
	resp, err := h.artifacts.SaveForTurn(ctx, &artifact.SaveRequest{
		AppName: artifactref.AppName, UserID: userID, SessionID: chatID, FileName: name,
		Part: &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: mimeType}},
	}, turnID)
	if err != nil {
		return nil, err
	}
	return artifactref.Encode(userID, chatID, name, resp.Version, mimeType), nil
}

// recoverRun keeps a panicking run from taking the process with it. All three
// run goroutines outlive the HTTP request, so chi's Recoverer never sees them -
// the gap that made #1033 fatal. Registered FIRST so it unwinds LAST, covering
// panics raised by the cleanup defers themselves.
func recoverRun(chatID schema.ChatID, turnID string) {
	if r := recover(); r != nil {
		slog.Error("chat run panicked; run abandoned, process survives",
			"component", "rest", "chat", chatID, "turn", turnID,
			"panic", r, "stack", string(debug.Stack()))
	}
}

// Launches a chat turn as server-side work on a server-lifetime context (not the HTTP request's). Outlives its initiating client.
func (h *Handler) startRun(chatID, turnID, content string, attachments []*genai.Part) {
	runCtx, cancelRun := context.WithTimeout(context.Background(), runTimeout)
	// Registered synchronously so cancel can never miss the run.
	h.hub.RegisterRun(chatID, turnID, cancelRun)
	// Marks the chat in-flight so a crash before stampRunOutcome runs is detectable (#738).
	_ = h.store.MarkRunActive(runCtx, chatID, turnID)
	go func() {
		defer recoverRun(chatID, turnID)
		// Close done LAST so the run is off the registry by the time viewers see the stream close.
		defer h.hub.Close(chatID)
		defer func() {
			cancelRun()
			h.hub.UnregisterRun(chatID)
		}()
		h.runChat(runCtx, chatID, turnID, content, attachments)
	}()
}

// Drives the orchestrator and publishes the SSE stream to the hub and durable event log. No HTTP client dependency.
func (h *Handler) runChat(runCtx context.Context, chatID, turnID, message string, attachments []*genai.Part) {
	// Stamps the outcome on every exit path, including the error return below (#738).
	defer h.stampRunOutcome(runCtx, chatID)
	// Clear previous run's durable events so this run's seq starts at 1.
	h.eventLog.Reset(runCtx, chatID)

	// pub assigns next per-chat seq, fans to hub subscribers, and persists durably.
	pub := runlog.NewPublisher(h.hub, h.eventLog, chatID)
	publish := pub.Publish

	publish(stream.ResponseCreated(turnID))

	titleCh := make(chan string, 1)
	go func() {
		defer close(titleCh)
		c, _ := h.store.GetChat(runCtx, chatID)
		if c == nil || c.Title != "" {
			return
		}
		title := h.generateTitle(runCtx, chatID, message)
		if title == "" {
			return
		}
		_ = h.store.UpdateTitle(runCtx, chatID, title)
		titleCh <- title
	}()

	trySendTitle := func() {
		select {
		case title, ok := <-titleCh:
			if ok {
				publish(stream.ChatTitle(title))
			}
		default:
		}
	}

	// res.Step is the same per-event bookkeeping runlog.Drive uses for the SDK
	// extension dispatch path - REST can't range through Drive directly (it
	// interleaves trySendTitle and aborts immediately on error), so it calls
	// the shared step function instead of hand-rolling its own copy.
	var res runlog.DriveResult
	for ev, err := range h.orch.Run(runCtx, h.sessionUser(runCtx, chatID), chatID, orchestrator.SourceApp, message, attachments) {
		trySendTitle()
		if err != nil {
			publish(stream.Errorf(err.Error()))
			publish(stream.Done())
			return
		}
		res.Step(h.store, chatID, turnID, true, ev)
		publish(ev)
	}
	for title := range titleCh {
		publish(stream.ChatTitle(title))
	}
	publish(stream.Done())
	// Stamp model + usage on the turn row - shared with the SDK extension
	// dispatch path (internal/serve.driveExtensionRunEvents) so both callers
	// stamp the same way instead of each hand-rolling it.
	runlog.StampTurn(runCtx, h.store, chatID, turnID, res)
}

// Cancels the chat's active run when response_id names it; stale ids 404.
func (h *Handler) UpdateResponseStatus(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, responseID schema.ResponseID) {
	var body schema.ResponseStatusUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errMsg(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Status != schema.Cancelled {
		errMsg(w, http.StatusBadRequest, "unsupported target status")
		return
	}
	if !h.hub.CancelResponse(chatID, responseID) {
		errMsg(w, http.StatusNotFound, "no active run with this response id")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loadPlanNode is the shared prologue for the node lifecycle endpoints:
// resolve the chat's latest plan, 404 unless nodeID is actually in it
// (GetDagNode returns (nil, nil) for a missing row, so the row alone cannot
// prove existence), and read the persisted status (missing row = queued).
// ok=false means the response has been written.
func (h *Handler) loadPlanNode(w http.ResponseWriter, r *http.Request, chatID, nodeID string) (dp *store.DagPlan, dn *store.DagNode, current dag.NodeStatus, ok bool) {
	dp, err := h.store.GetLatestDagPlan(r.Context(), chatID)
	if err != nil || dp == nil {
		errMsg(w, http.StatusNotFound, "no plan for this chat")
		return nil, nil, "", false
	}
	var planData stream.DagPlanData
	if err := json.Unmarshal([]byte(dp.PlanJSON), &planData); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return nil, nil, "", false
	}
	nodeFound := false
	for _, n := range planData.Nodes {
		if n.ID == nodeID {
			nodeFound = true
			break
		}
	}
	if !nodeFound {
		errMsg(w, http.StatusNotFound, "no such node in the plan")
		return nil, nil, "", false
	}
	dn, err = h.store.GetDagNode(r.Context(), dp.ID, nodeID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return nil, nil, "", false
	}
	current = dag.StatusQueued
	if dn != nil {
		current = dag.NodeStatus(dn.Status)
	}
	return dp, dn, current, true
}

// wireStatus maps the legacy needs_input spelling to paused, the same
// normalization dagNodeState applies - 409 bodies must speak it too.
func wireStatus(s dag.NodeStatus) schema.NodeStatus {
	if s == dag.StatusNeedsInput {
		return schema.NodeStatusPaused
	}
	return schema.NodeStatus(s)
}

// Transitions one DAG node's status via dag.CanTransition; illegal transitions 409 with the allowed targets.
func (h *Handler) UpdateNodeStatus(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.NodeStatusUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errMsg(w, http.StatusBadRequest, "invalid request body")
		return
	}
	guidance := ""
	if body.Guidance != nil {
		guidance = strings.TrimSpace(*body.Guidance)
	}
	target := dag.NodeStatus(body.Status)

	dp, dn, current, ok := h.loadPlanNode(w, r, chatID, nodeID)
	if !ok {
		return
	}

	if !dag.CanTransition(current, target) {
		writeJSON(w, http.StatusConflict, schema.TransitionError{
			Error:   fmt.Sprintf("illegal transition: %s -> %s", current, target),
			Current: wireStatus(current),
			Allowed: allowedStatuses(current),
		})
		return
	}

	switch target {
	case dag.StatusCancelled:
		// Not optimistic: CancelNode returns false when no live control is registered.
		// A delivered cancel is cooperative - the node stops at its next stage boundary.
		if !h.orch.CancelNode(chatID, nodeID) {
			writeJSON(w, http.StatusConflict, schema.TransitionError{
				Error:   "node is not cancellable right now (no live run - it may be queued but not yet dispatched, or already finished); nothing was cancelled",
				Current: wireStatus(current),
				Allowed: allowedStatuses(current),
			})
			return
		}
		writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusCancelled))
	case dag.StatusPaused:
		reason := dag.PauseUser
		if body.Reason != nil && *body.Reason != "" {
			reason = dag.PauseReason(*body.Reason)
		}
		// awaiting_input is system-owned (the worker's interrupt sets it, and
		// markPaused treats it specially); clients may only pause as user/shutdown.
		if reason != dag.PauseUser && reason != dag.PauseShutdown {
			errMsg(w, http.StatusBadRequest, "reason must be \"user\" or \"shutdown\"")
			return
		}
		if !h.orch.PauseNode(chatID, nodeID, reason) {
			writeJSON(w, http.StatusConflict, schema.TransitionError{
				Error:   "node is not pausable right now (no live run); nothing was paused",
				Current: wireStatus(current),
				Allowed: allowedStatuses(current),
			})
			return
		}
		writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusPaused))
	case dag.StatusRunning:
		// paused → running: a fresh re-run reusing the plan's stored outputs.
		if h.hub.Draining() {
			errMsg(w, http.StatusServiceUnavailable, "server is shutting down; try again shortly")
			return
		}
		h.retryNodeAsync(dp, chatID, nodeID, guidance)
		writeJSON(w, http.StatusOK, schema.DagNodeState{Status: schema.NodeStatusQueued})
	case dag.StatusQueued:
		if h.hub.Draining() {
			errMsg(w, http.StatusServiceUnavailable, "server is shutting down; try again shortly")
			return
		}
		h.retryNodeAsync(dp, chatID, nodeID, guidance)
		writeJSON(w, http.StatusOK, schema.DagNodeState{Status: schema.NodeStatusQueued})
	}
}

// StartNode is the explicit per-node "start" transition (#962): queued or
// paused -> running. For a node paused awaiting_input, body.Content is the
// answer to its parked question, delivered the same way SendChatMessage
// delivers a chat-level answer.
func (h *Handler) StartNode(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.NodeStartBody
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // optional body - no answer needed outside awaiting_input
	}
	dp, dn, current, ok := h.loadPlanNode(w, r, chatID, nodeID)
	if !ok {
		return
	}
	if !dag.CanTransition(current, dag.StatusRunning) {
		writeJSON(w, http.StatusConflict, schema.TransitionError{
			Error:   fmt.Sprintf("illegal transition: %s -> running", current),
			Current: wireStatus(current),
			Allowed: allowedStatuses(current),
		})
		return
	}
	if h.hub.Draining() {
		errMsg(w, http.StatusServiceUnavailable, "server is shutting down; try again shortly")
		return
	}
	content := ""
	if body.Content != nil {
		content = strings.TrimSpace(*body.Content)
	}
	awaiting := dn != nil && (current == dag.StatusNeedsInput || dag.PauseReason(dn.PauseReason) == dag.PauseAwaitingInput)
	if awaiting && content == "" {
		errMsg(w, http.StatusBadRequest, "content is required: this node is paused on a question and must not resume with a blank answer")
		return
	}
	// Only a HITL park re-enters via Orchestrator.StartNode (the answer must
	// reach ADK's Resume). Any other start is the scoped node+descendants
	// re-run - a full-plan re-entry would re-execute done siblings (#964).
	if awaiting {
		h.startNodeAsync(dp, chatID, nodeID, content)
	} else {
		h.retryNodeAsync(dp, chatID, nodeID, "")
	}
	writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusRunning))
}

// StopNode is the explicit per-node "stop" transition: any non-terminal
// status -> cancelled. Cooperative for a running node; immediate otherwise.
func (h *Handler) StopNode(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	dp, dn, current, ok := h.loadPlanNode(w, r, chatID, nodeID)
	if !ok {
		return
	}
	if !dag.CanTransition(current, dag.StatusCancelled) {
		writeJSON(w, http.StatusConflict, schema.TransitionError{
			Error:   fmt.Sprintf("illegal transition: %s -> cancelled", current),
			Current: wireStatus(current),
			Allowed: allowedStatuses(current),
		})
		return
	}
	if !h.orch.StopNode(chatID, nodeID) {
		// A parked node has no live control (it unregisters when the gate
		// closure returns), so cancel it on the row directly - otherwise the
		// legal paused -> cancelled transition is unreachable.
		if dag.IsPaused(current) {
			if err := h.store.SetNodeStatus(r.Context(), dp.ID, nodeID, dag.StatusCancelled, "", ""); err != nil {
				httpError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusCancelled))
			return
		}
		writeJSON(w, http.StatusConflict, schema.TransitionError{
			Error:   "node is not stoppable right now (no live run - it may be queued but not yet dispatched, or already finished); nothing was stopped",
			Current: wireStatus(current),
			Allowed: allowedStatuses(current),
		})
		return
	}
	writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusCancelled))
}

// Appends a message to a running node's queue, delivered at its next turn boundary. 404s if the node isn't live.
func (h *Handler) QueueNodeMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.QueueMessageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		errMsg(w, http.StatusBadRequest, "message is required")
		return
	}
	m, ok := h.orch.QueueNodeMessage(chatID, nodeID, strings.TrimSpace(body.Message))
	if !ok {
		errMsg(w, http.StatusNotFound, "node is not running right now; the message has nowhere to land")
		return
	}
	writeJSON(w, http.StatusOK, queuedMessageWire(m))
}

// Rewrites a not-yet-delivered queued message.
func (h *Handler) EditQueuedMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID, messageID schema.MessageID) {
	var body schema.QueueMessageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		errMsg(w, http.StatusBadRequest, "message is required")
		return
	}
	if !h.orch.EditQueuedMessage(chatID, nodeID, messageID, strings.TrimSpace(body.Message)) {
		errMsg(w, http.StatusConflict, "no such pending queued message (unknown, or already delivered)")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Drops a not-yet-delivered queued message.
func (h *Handler) RemoveQueuedMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID, messageID schema.MessageID) {
	if !h.orch.RemoveQueuedMessage(chatID, nodeID, messageID) {
		errMsg(w, http.StatusConflict, "no such pending queued message (unknown, or already delivered)")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Replaces a not-yet-started node's task text. 409 once the node has started.
func (h *Handler) EditNodeTask(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.EditNodeTaskBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Task) == "" {
		errMsg(w, http.StatusBadRequest, "task is required")
		return
	}
	if !h.orch.SetNodeTaskOverride(chatID, nodeID, body.Task) {
		errMsg(w, http.StatusConflict, "the node has already started; its prompt is immutable")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func queuedMessageWire(m dag.QueuedMessage) schema.QueuedMessage {
	return schema.QueuedMessage{
		Id:        m.ID,
		Text:      m.Text,
		Delivered: m.Delivered,
		CreatedAt: m.CreatedAt,
	}
}

// Builds a PUT node-status response body from the persisted row, with Status overridden to the accepted target.
func optimisticNodeState(dn *store.DagNode, target dag.NodeStatus) schema.DagNodeState {
	var ns schema.DagNodeState
	if dn != nil {
		ns = dagNodeState(*dn)
	}
	ns.Status = schema.NodeStatus(target)
	if target != dag.StatusPaused {
		// The spec scopes these to paused nodes; don't echo stale row values.
		ns.PauseReason, ns.PendingQuestion = nil, nil
	}
	return ns
}

// allowedStatuses converts dag.AllowedTargets to the wire enum for a 409 body.
func allowedStatuses(from dag.NodeStatus) []schema.NodeStatus {
	var out []schema.NodeStatus
	seen := map[schema.NodeStatus]bool{}
	for _, t := range dag.AllowedTargets(from) {
		// One wire vocabulary: needs_input is served as paused everywhere else.
		w := wireStatus(t)
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// Starts (or resumes) nodeID in background via Orchestrator.StartNode - a
// fresh dispatch from queued, or a re-entry at the node's last gate boundary
// from paused, delivering message as the parked question's answer when the
// node paused awaiting_input. Progress publishes through the same hub as
// retryNodeAsync.
func (h *Handler) startNodeAsync(dp *store.DagPlan, chatID, nodeID, message string) {
	go func() {
		runCtx, cancelRun := context.WithTimeout(context.Background(), runTimeout)
		h.hub.RegisterRun(chatID, dp.TurnID, cancelRun)
		_ = h.store.MarkRunActive(runCtx, chatID, dp.TurnID)
		defer recoverRun(chatID, dp.TurnID)
		defer func() {
			cancelRun()
			h.hub.UnregisterRun(chatID)
		}()
		defer h.hub.Close(chatID)
		defer h.stampRunOutcome(runCtx, chatID)

		h.eventLog.Reset(runCtx, chatID)
		publish := runlog.NewPublisher(h.hub, h.eventLog, chatID).Publish
		publish(stream.ResponseCreated(dp.TurnID))

		userID := h.sessionUser(runCtx, chatID)
		for ev, err := range iterFromStart(runCtx, h.orch, userID, chatID, nodeID, message) {
			if err != nil {
				publish(stream.Errorf(err.Error()))
				break
			}
			runlog.PersistNodeEvent(h.store, dp.ID, ev)
			publish(ev)
		}
		publish(stream.Done())
	}()
}

// iterFromStart adapts Orchestrator.StartNode's yield-callback shape to the
// iter.Seq2 the other node-run helpers range over.
func iterFromStart(ctx context.Context, o *orchestrator.Orchestrator, userID, chatID, nodeID, message string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		o.StartNode(ctx, userID, chatID, nodeID, message, yield)
	}
}

// Re-runs nodeID and descendants in background, reusing the plan's stored outputs. Progress publishes through the same hub.
func (h *Handler) retryNodeAsync(dp *store.DagPlan, chatID, nodeID, guidance string) {
	nodes, _ := h.store.GetDagNodes(context.Background(), dp.ID)
	seeded := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.Output != "" {
			seeded[n.NodeID] = n.Output
		}
	}
	go func() {
		runCtx, cancelRun := context.WithTimeout(context.Background(), runTimeout)
		h.hub.RegisterRun(chatID, dp.TurnID, cancelRun)
		_ = h.store.MarkRunActive(runCtx, chatID, dp.TurnID)
		defer recoverRun(chatID, dp.TurnID)
		defer func() {
			cancelRun()
			h.hub.UnregisterRun(chatID)
		}()
		defer h.hub.Close(chatID)
		defer h.stampRunOutcome(runCtx, chatID)

		h.eventLog.Reset(runCtx, chatID)
		publish := runlog.NewPublisher(h.hub, h.eventLog, chatID).Publish
		publish(stream.ResponseCreated(dp.TurnID))

		for ev, err := range h.orch.RetryNode(runCtx, h.sessionUser(runCtx, chatID), chatID, seeded, nodeID, guidance) {
			if err != nil {
				publish(stream.Errorf(err.Error()))
				break
			}
			runlog.PersistNodeEvent(h.store, dp.ID, ev) // update the re-run nodes' persisted state
			publish(ev)
		}
		publish(stream.Done())
	}()
}

// Connects a client to a chat's live (or just-completed) run. Reconnect-safe via Last-Event-ID or the durable event log.
func (h *Handler) SubscribeChatStream(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	if !h.requireChat(w, r, chatID) {
		return
	}
	sse, ok := newSSEWriter(w)
	if !ok {
		errMsg(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	lastSeq := lastEventID(r)
	// Covers all drivers of a run on this chat (REST or GitHub-dispatched).
	active := h.hub.Active(chatID)
	replay, live, cancel, done := h.hub.Subscribe(chatID)
	defer cancel()

	// Cold path: hub has no buffered events - replay from the durable log.
	if len(replay) == 0 && !active {
		evs, err := h.store.LoadChatEvents(r.Context(), chatID, lastSeq)
		if err != nil {
			slog.Warn("subscribe: durable replay failed", "component", "stream", "chat", chatID, "err", err)
			return
		}
		for _, e := range evs {
			ev, err := runlog.UnmarshalEvent(e.Event)
			if err != nil {
				continue
			}
			if sse.sendID(e.Seq, ev) != nil {
				return
			}
		}
		return
	}

	// Warm path: hub holds the run. Replay buffer (skipping what the client has seen), then tail live.
	if done {
		for _, it := range replay {
			if it.Seq > lastSeq {
				if sse.sendID(it.Seq, it.SSE) != nil {
					return
				}
			}
		}
		return
	}
	streamHub(r.Context(), sse, replay, live, lastSeq)
}

// Forwards one run's events to one client: replay buffer, then live tail. The ONLY place events touch an HTTP connection.
func streamHub(ctx context.Context, sse *sseWriter, replay []stream.Event, live <-chan stream.Event, lastSeq int64) {
	send := func(it stream.Event) bool {
		if it.Seq <= lastSeq {
			return true
		}
		return sse.sendID(it.Seq, it.SSE) == nil
	}
	for _, it := range replay {
		if !send(it) {
			return
		}
	}
	for {
		select {
		case it, ok := <-live:
			if !ok {
				return // run ended (its Done was delivered via the live channel)
			}
			if !send(it) {
				return
			}
		case <-ctx.Done():
			return // this client is gone; the run carries on without it
		}
	}
}

// Reads a reconnecting subscriber's last-seen seq from Last-Event-ID header or last_event_id query param.
func lastEventID(r *http.Request) int64 {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("last_event_id")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func buildTurn(tc store.TurnContent) schema.Turn {
	// planData is the turn's DAG shape; unmarshaled once for both the DAG
	// output item and the answer bubble's text below.
	var planData stream.DagPlanData
	planOK := tc.Plan != nil && json.Unmarshal([]byte(tc.Plan.PlanJSON), &planData) == nil

	// DAG turns: the answer bubble carries the terminal node's OUTPUT - what
	// the live stream rendered (chatStore's liveDagFinalText) - not the
	// orchestrator's planning narration. Persisting that narration made a
	// reload swap the bubble for the chatter the live view already discards,
	// so a review read differently after refresh than it did live.
	bubbleText := tc.AsstText
	if planOK {
		if out := terminalNodeOutput(planData, tc.Nodes); out != "" {
			bubbleText = out
		}
	}

	var msgItem schema.OutputItem
	if bubbleText != "" || tc.AsstThink != "" {
		content := make([]schema.ContentPart, 0, 2)
		if tc.AsstThink != "" {
			var cp schema.ContentPart
			_ = cp.FromReasoningPart(schema.ReasoningPart{Text: tc.AsstThink})
			content = append(content, cp)
		}
		if bubbleText != "" {
			var cp schema.ContentPart
			_ = cp.FromOutputTextPart(schema.OutputTextPart{Text: bubbleText})
			content = append(content, cp)
		}
		_ = msgItem.FromMessageOutputItem(schema.MessageOutputItem{
			Id:      tc.ID + ":msg",
			Status:  schema.Completed,
			Content: content,
		})
	}

	var dagItem *schema.OutputItem
	if planOK {
		nodes := make([]schema.DagNodeDef, len(planData.Nodes))
		for i, n := range planData.Nodes {
			nodes[i] = schema.DagNodeDef{Id: n.ID, Agent: n.Agent, Task: n.Task, DependsOn: n.DependsOn, ContextWindow: intPtr(n.ContextWindow)}
		}
		edges := make([]schema.DagEdge, len(planData.Edges))
		for i, e := range planData.Edges {
			edges[i] = schema.DagEdge{From: e.From, To: e.To}
		}
		nodeStates := make(map[string]schema.DagNodeState, len(tc.Nodes))
		for _, n := range tc.Nodes {
			nodeStates[n.NodeID] = dagNodeState(n)
		}
		// Completed if all nodes are done/failed/cancelled, in_progress otherwise.
		dagStatus := schema.Completed
		for _, ns := range nodeStates {
			if ns.Status == schema.NodeStatusRunning || ns.Status == schema.NodeStatusQueued || ns.Status == schema.NodeStatusNeedsInput || ns.Status == schema.NodeStatusPaused {
				dagStatus = schema.InProgress
				break
			}
		}
		item := new(schema.OutputItem)
		_ = item.FromDagOutputItem(schema.DagOutputItem{
			Id:         tc.Plan.ID,
			Status:     dagStatus,
			PlanId:     tc.Plan.ID,
			Nodes:      nodes,
			Edges:      edges,
			NodeStates: nodeStates,
		})
		dagItem = item
	}

	var activityItem *schema.OutputItem
	if len(tc.ToolCalls) > 0 {
		calls := make([]schema.ToolCallItem, len(tc.ToolCalls))
		for i, c := range tc.ToolCalls {
			calls[i] = schema.ToolCallItem{CallId: c.CallID, Name: c.Name}
			if c.Args != nil {
				calls[i].Args = &c.Args
			}
			if c.Result != nil {
				calls[i].Result = &c.Result
			}
		}
		oi := new(schema.OutputItem)
		_ = oi.FromAgentActivityOutputItem(schema.AgentActivityOutputItem{
			Id:        tc.ID + ":activity",
			Status:    schema.Completed,
			ToolCalls: calls,
		})
		activityItem = oi
	}

	output := make([]schema.OutputItem, 0, 3)
	if activityItem != nil {
		output = append(output, *activityItem)
	}
	if dagItem != nil {
		output = append(output, *dagItem)
	}
	if bubbleText != "" || tc.AsstThink != "" {
		output = append(output, msgItem)
	}

	// Orchestrator's token usage (UsageMetadata survives ADK's round-trip; ModelVersion doesn't, hence the separate model field).
	var usage *schema.Usage
	if tc.PromptTokens > 0 || tc.CompletionTokens > 0 || tc.ReasoningTokens > 0 {
		usage = &schema.Usage{
			InputTokens:  intPtr(int(tc.PromptTokens)),
			OutputTokens: intPtr(int(tc.CompletionTokens + tc.ReasoningTokens)),
		}
		if tc.CachedTokens > 0 {
			usage.CachedTokens = intPtr(int(tc.CachedTokens))
		}
	}

	return schema.Turn{
		Id:        tc.ID,
		CreatedAt: tc.CreatedAt,
		Input:     schema.TurnInput{Role: schema.TurnInputRoleUser, Content: tc.UserText},
		Output:    output,
		Usage:     usage,
		// Persisted on the turn row at run end (ADK drops ModelVersion). Nil for DAG turns.
		Model: strPtr(tc.Model),
	}
}

// Converts persisted store.DagNode into wire DagNodeState. Shared by buildTurn and UpdateNodeStatus.
// Normalizes the legacy needs_input DB spelling to the one wire vocabulary
// the SPA sees: paused, with pause_reason awaiting_input - dag.IsPaused
// covers both spellings; the DB column itself is untouched.
func dagNodeState(n store.DagNode) schema.DagNodeState {
	status := dag.NodeStatus(n.Status)
	reason := n.PauseReason
	if status == dag.StatusNeedsInput {
		status = dag.StatusPaused
		reason = string(dag.PauseAwaitingInput)
	}
	ns := schema.DagNodeState{
		Status:           schema.NodeStatus(status),
		Model:            strPtr(n.Model),
		FinishReason:     strPtr(n.FinishReason),
		OutputPreview:    strPtr(n.OutputPreview),
		Error:            strPtr(n.Error),
		PromptTokens:     intPtr(int(n.PromptTokens)),
		CompletionTokens: intPtr(int(n.CompletionTokens)),
		ReasoningTokens:  intPtr(int(n.ReasoningTokens)),
		TotalTokens:      intPtr(int(n.TotalTokens)),
		CachedTokens:     intPtr(int(n.CachedTokens)),
		ServerDurationMs: intPtr(int(n.DurationMs)),
		JudgeRounds:      intPtr(int(n.JudgeRounds)),
		JudgeFinalScore:  float64Ptr(n.JudgeFinalScore),
		JudgePassed:      boolPtr(n.JudgePassed),
	}
	if reason != "" {
		pr := schema.PauseReason(reason)
		ns.PauseReason = &pr
	}
	if n.PendingQuestion != "" {
		ns.PendingQuestion = strPtr(n.PendingQuestion)
	}
	if n.QueuedMessages != "" {
		var q []dag.QueuedMessage
		if err := json.Unmarshal([]byte(n.QueuedMessages), &q); err == nil && len(q) > 0 {
			qm := make([]schema.QueuedMessage, len(q))
			for i, m := range q {
				qm[i] = queuedMessageWire(m)
			}
			ns.QueuedMessages = &qm
		}
	}
	if n.StartedAt != nil {
		ms := int(n.StartedAt.UnixMilli())
		ns.StartedAtMs = &ms
	}
	if n.FinishedAt != nil {
		ms := int(n.FinishedAt.UnixMilli())
		ns.FinishedAtMs = &ms
	}
	return ns
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func intPtr(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func boolPtr(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

func float64Ptr(f float64) *float64 {
	if f == 0 {
		return nil
	}
	return &f
}

// chatTotalTokens is the single-chat convenience wrapper around
// ChatsUsageTotals, for call sites that only ever need one chat's total.
func (h *Handler) chatTotalTokens(ctx context.Context, chatID string) int64 {
	totals, err := h.store.ChatsUsageTotals(ctx, []string{chatID})
	if err != nil {
		slog.Warn("chat usage totals failed", "component", "rest", "chat", chatID, "err", err)
		return 0
	}
	return totals[chatID]
}

// Builds a ChatSummary from the chat row alone: queued/running are cheap in-memory checks,
// everything else is the stamp StampRunOutcome left at the last run's end - no turns/session
// read per chat (#738; that per-chat read is what GetChat's chatStatus below still does,
// which is fine there since GetChat already loads turns for the full detail body).
// totalTokens is the chat's compact token count for the sidebar (see
// ChatsUsageTotals) - 0 for a brand-new chat with no run yet.
func (h *Handler) toSummary(c store.Chat, totalTokens int64) schema.ChatSummary {
	status, pendingQuestion := h.liveOrStampedStatus(c)
	s := schema.ChatSummary{
		Id:              c.ID,
		Title:           strPtr(c.Title),
		SystemPrompt:    c.SystemPrompt,
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
		Status:          status,
		PendingQuestion: pendingQuestion,
		GithubUrl:       strPtr(c.GithubURL),
		GithubRepo:      strPtr(c.GithubRepo),
		GithubState:     stateVal(c.GithubState),
		Archived:        boolPtr(c.Archived),
		Origin:          chatOrigin(c.Origin),
	}
	if totalTokens > 0 {
		s.TotalTokens = intPtr(int(totalTokens))
	}
	return s
}

// liveOrStampedStatus resolves queued/running live, else the chat row's stamped outcome.
// A non-empty ActiveTurnID with no live signal means the run that set it died before
// StampRunOutcome could clear it - report failed rather than trust a stale idle/needs_input
// stamp from a run before that one (#738 test 3; single-instance Hub, see stream.NewHub).
func (h *Handler) liveOrStampedStatus(c store.Chat) (schema.ChatStatus, *string) {
	if h.orch.Queued(c.ID) {
		return schema.ChatStatusQueued, nil
	}
	if h.hub.Active(c.ID) {
		return schema.ChatStatusRunning, nil
	}
	if c.ActiveTurnID != "" {
		return schema.ChatStatusFailed, nil
	}
	switch c.RunStatus {
	case store.RunStatusNeedsInput:
		q := c.PendingQuestion
		return schema.ChatStatusNeedsInput, &q
	case store.RunStatusFailed, store.RunStatusInterrupted:
		return schema.ChatStatusFailed, nil
	default:
		return schema.ChatStatusIdle, nil
	}
}

// Computes a chat's LIVE derived status: queued (waiting on max_active_runs), running (hub
// has a live run), needs_input (prior events end on an unanswered question), failed (last
// DAG node failed, no answer), or idle. Used by GetChat, which loads turns regardless for
// the detail body, so this per-chat computation costs nothing extra there.
func (h *Handler) chatStatus(ctx context.Context, chatID string, turns []store.TurnContent) (schema.ChatStatus, *string) {
	if h.orch.Queued(chatID) {
		return schema.ChatStatusQueued, nil
	}
	if h.hub.Active(chatID) {
		return schema.ChatStatusRunning, nil
	}
	return h.terminalStatus(ctx, chatID, turns)
}

// terminalStatus is chatStatus without the live queued/running checks: the outcome a run
// stamps on its chat row once it ends (#738).
func (h *Handler) terminalStatus(ctx context.Context, chatID string, turns []store.TurnContent) (schema.ChatStatus, *string) {
	q, hasQ := h.orch.PendingQuestion(ctx, h.sessionUser(ctx, chatID), chatID)
	status, question := store.DeriveTerminalStatus(turns, q, hasQ)
	if status == store.RunStatusNeedsInput {
		return schema.ChatStatusNeedsInput, &question
	}
	return schema.ChatStatus(status), nil
}

// stampRunOutcome persists a finished run's terminal status on the chat row so ListChats can
// read it directly (#738). Call at every run-end path (defer, so it fires on error too) -
// only a hard process crash skips it, and ActiveTurnID (MarkRunActive) covers that case.
// Detached from parent so a mid-run cancel can't also cancel the stamp write.
func (h *Handler) stampRunOutcome(parent context.Context, chatID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	// Shutdown force-cancelled this run - its nodes are paused/shutdown on
	// disk (#962) and boot resumes them, so the chat stamps paused, not
	// whatever DeriveTerminalStatus would guess from a cut-off turn.
	if h.hub.WasInterrupted(chatID) {
		if err := h.store.StampRunOutcome(ctx, chatID, store.RunStatusPaused, ""); err != nil {
			slog.Warn("stamp run outcome: paused persist failed", "component", "rest", "chat", chatID, "err", err)
		}
		return
	}
	turns, err := h.store.GetTurnsWithContent(ctx, orchestrator.AppName, h.sessionUser(ctx, chatID), chatID)
	if err != nil {
		slog.Warn("stamp run outcome: turns load failed", "component", "rest", "chat", chatID, "err", err)
	}
	status, pendingQuestion := h.terminalStatus(ctx, chatID, turns)
	q := ""
	if pendingQuestion != nil {
		q = *pendingQuestion
	}
	if err := h.store.StampRunOutcome(ctx, chatID, string(status), q); err != nil {
		slog.Warn("stamp run outcome failed", "component", "rest", "chat", chatID, "err", err)
	}
}

func stateVal(s string) *schema.ChatSummaryGithubState {
	if s == "" {
		return nil
	}
	v := schema.ChatSummaryGithubState(s)
	return &v
}

func detailStateVal(s string) *schema.ChatDetailGithubState {
	if s == "" {
		return nil
	}
	v := schema.ChatDetailGithubState(s)
	return &v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// httpError writes err as the JSON schema.ErrorResponse body every 4xx/5xx
// response declares - the one place a plain-text http.Error used to fire.
func httpError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, schema.ErrorResponse{Error: err.Error()})
}

// errMsg writes msg as a schema.ErrorResponse - httpError's counterpart for
// the call sites that only ever had a literal string, not an error value.
func errMsg(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, schema.ErrorResponse{Error: msg})
}

// requireChat 404s (writing the response) and returns false if chatID names
// no chat - the preflight send/subscribe run BEFORE opening an SSE stream, so
// a bad chat_id is a clean 404, never an in-stream error event.
func (h *Handler) requireChat(w http.ResponseWriter, r *http.Request, chatID string) bool {
	c, err := h.store.GetChat(r.Context(), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return false
	}
	if c == nil {
		errMsg(w, http.StatusNotFound, "no such chat")
		return false
	}
	return true
}

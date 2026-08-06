// Package rest implements the generated OpenAPI ServerInterface for Quack's REST surface.
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
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
	jail         *workspace.Jail    // per-chat workspace tree cleanup on delete; nil ⇒ no workspace configured
	hub          *stream.Hub        // fans a chat's run to extra subscribers (other devices); also the cancel-run registry (#468)
	eventLog     *runlog.EventLog   // durably persists the run stream, backing replay across restarts
	ledgerStore  ledger.LedgerStore // replay ledger backend; nil ⇒ recording disabled, GetChatRecording 404s
	quackVersion string             // build stamp, stamped into a recording bundle's manifest.json
}

// NewHandler builds a REST handler. jail/hub/ledgerStore may be nil; hub defaults to a private hub.
func NewHandler(s *store.Store, o *orchestrator.Orchestrator, titler model.LLM, jail *workspace.Jail, hub *stream.Hub, ledgerStore ledger.LedgerStore, quackVersion string) *Handler {
	if hub == nil {
		hub = stream.NewHub()
	}
	return &Handler{store: s, orch: o, titler: titler, jail: jail, hub: hub, eventLog: runlog.NewEventLog(s), ledgerStore: ledgerStore, quackVersion: quackVersion}
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

func (h *Handler) ListChats(w http.ResponseWriter, r *http.Request, params schema.ListChatsParams) {
	limit := 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	pageToken := ""
	if params.PageToken != nil {
		pageToken = *params.PageToken
	}
	chats, next, err := h.store.ListChats(r.Context(), limit, pageToken)
	if err != nil {
		if errors.Is(err, store.ErrInvalidPageToken) {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	// Fan out status reads (serial took 3-4s for 15 chats).
	// ponytail: still N+1 reads, just concurrent; the real fix is stamping the
	// run outcome on the chat row at run END so list is one table read.
	out := schema.ChatList{Data: make([]schema.ChatSummary, len(chats))}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, c := range chats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out.Data[i] = h.toSummary(r.Context(), c)
		}()
	}
	wg.Wait()
	if next != "" {
		out.NextPageToken = &next
	}
	writeJSON(w, http.StatusOK, out)
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
	writeJSON(w, http.StatusOK, h.toSummary(r.Context(), *c))
}

func (h *Handler) GetChat(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	c, err := h.store.GetChat(r.Context(), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if c == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	turns, err := h.store.GetTurnsWithContent(r.Context(), orchestrator.AppName, store.SessionUserFor(*c), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	status, pendingQuestion := h.chatStatus(r.Context(), chatID, turns)
	detail := schema.ChatDetail{
		Id:              c.ID,
		Title:           nonEmpty(c.Title),
		SystemPrompt:    c.SystemPrompt,
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
		Status:          status,
		PendingQuestion: pendingQuestion,
		Turns:           make([]schema.Turn, 0, len(turns)),
	}
	for _, tc := range turns {
		detail.Turns = append(detail.Turns, buildTurn(tc))
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *Handler) GetResponse(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, responseID schema.ResponseID) {
	tc, err := h.store.GetTurnWithContent(r.Context(), orchestrator.AppName, h.sessionUser(r.Context(), chatID), chatID, responseID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if tc == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, buildTurn(*tc))
}

// Lists sessions the replay ledger has entries for (backs `quack recording list`).
func (h *Handler) ListRecordings(w http.ResponseWriter, r *http.Request) {
	if h.ledgerStore == nil {
		http.Error(w, "recording is not enabled", http.StatusNotFound)
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
		http.Error(w, "recording is not enabled", http.StatusNotFound)
		return
	}
	entries, err := h.ledgerStore.ReadStream(r.Context(), chatID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "no recording for this chat", http.StatusNotFound)
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

// Partial update to a chat's mutable metadata (Title is the only settable field today).
func (h *Handler) UpdateChat(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	var body schema.UpdateChatBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Title == nil || strings.TrimSpace(*body.Title) == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	c, err := h.store.GetChat(r.Context(), chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if c == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	title := strings.TrimSpace(*body.Title)
	if err := h.store.UpdateTitle(r.Context(), chatID, title); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	c.Title = title
	writeJSON(w, http.StatusOK, h.toSummary(r.Context(), *c))
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
	var body schema.SendMessageBody
	var attachments []*genai.Part

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "invalid multipart body", http.StatusBadRequest)
			return
		}
		body.Content = r.FormValue("content")
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
				mime := fh.Header.Get("Content-Type")
				if mime == "" {
					mime = "application/octet-stream"
				}
				attachments = append(attachments, &genai.Part{
					InlineData: &genai.Blob{Data: data, MIMEType: mime},
				})
			}
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == "" {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	if body.Content == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Generate a stable turn ID before the run so the DAG plan can reference it, and the cancel endpoint can name it.
	turnID := uuid.NewString()
	go func() { _ = h.store.SaveTurn(context.Background(), chatID, turnID) }()

	// Subscribe BEFORE the run starts publishing so nothing is missed.
	h.hub.Reset(chatID)
	replay, live, unsubscribe, _ := h.hub.Subscribe(chatID)
	defer unsubscribe()

	h.startRun(chatID, turnID, body.Content, attachments)

	// From here this handler is only a viewer - it cannot stall or kill the run.
	streamHub(r.Context(), sse, replay, live, 0)
}

// Launches a chat turn as server-side work on a server-lifetime context (not the HTTP request's). Outlives its initiating client.
func (h *Handler) startRun(chatID, turnID, content string, attachments []*genai.Part) {
	runCtx, cancelRun := context.WithTimeout(context.Background(), runTimeout)
	// Registered synchronously so cancel can never miss the run.
	h.hub.RegisterRun(chatID, turnID, cancelRun)
	go func() {
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

	var activePlanID string
	// Captured from top-level agent_complete so history can recover the model (ADK drops ModelVersion).
	var orchModel string

	for ev, err := range h.orch.Run(runCtx, h.sessionUser(runCtx, chatID), chatID, message, attachments) {
		trySendTitle()
		if err != nil {
			publish(stream.Errorf(err.Error()))
			publish(stream.Done())
			return
		}
		if ev.Name == stream.EventAgentComplete {
			if d, ok := ev.Data.(stream.AgentCompleteData); ok && d.NodeID == "" && d.Model != "" {
				orchModel = d.Model
			}
		}
		if ev.Name == stream.EventDagPlan {
			if d, ok := ev.Data.(stream.DagPlanData); ok {
				activePlanID = d.PlanID
				runlog.SaveDagPlan(h.store, chatID, turnID, d)
			}
		} else if activePlanID != "" {
			runlog.PersistNodeEvent(h.store, activePlanID, ev)
		}
		publish(ev)
	}
	for title := range titleCh {
		publish(stream.ChatTitle(title))
	}
	publish(stream.Done())
	// Stamp model on turn row (DAG turns credit models per-node on DagNodeState).
	if orchModel != "" && activePlanID == "" {
		_ = h.store.SetTurnModel(runCtx, chatID, turnID, orchModel)
	}
	_ = h.store.Touch(runCtx, chatID)
}

// Cancels the chat's active run when response_id names it; stale ids 404.
func (h *Handler) UpdateResponseStatus(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, responseID schema.ResponseID) {
	var body schema.ResponseStatusUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Status != schema.Cancelled {
		http.Error(w, "unsupported target status", http.StatusBadRequest)
		return
	}
	if !h.hub.CancelResponse(chatID, responseID) {
		http.Error(w, "no active run with this response id", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Transitions one DAG node's status via dag.CanTransition; illegal transitions 409 with the allowed targets.
func (h *Handler) UpdateNodeStatus(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.NodeStatusUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	guidance := ""
	if body.Guidance != nil {
		guidance = strings.TrimSpace(*body.Guidance)
	}
	target := dag.NodeStatus(body.Status)

	// Load plan/node defs (to confirm node exists) and persisted outputs a retry reuses.
	dp, err := h.store.GetLatestDagPlan(r.Context(), chatID)
	if err != nil || dp == nil {
		http.Error(w, "no plan for this chat", http.StatusNotFound)
		return
	}
	var planData stream.DagPlanData
	if err := json.Unmarshal([]byte(dp.PlanJSON), &planData); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	nodeFound := false
	for _, n := range planData.Nodes {
		if n.ID == nodeID {
			nodeFound = true
			break
		}
	}
	if !nodeFound {
		http.Error(w, "no such node in the plan", http.StatusNotFound)
		return
	}

	dn, err := h.store.GetDagNode(r.Context(), dp.ID, nodeID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	current := dag.StatusQueued
	if dn != nil {
		current = dag.NodeStatus(dn.Status)
	}

	if !dag.CanTransition(current, target) {
		writeJSON(w, http.StatusConflict, schema.TransitionError{
			Error:   fmt.Sprintf("illegal transition: %s -> %s", current, target),
			Current: schema.NodeStatus(current),
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
				Current: schema.NodeStatus(current),
				Allowed: allowedStatuses(current),
			})
			return
		}
		writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusCancelled))
	case dag.StatusPaused:
		if !h.orch.PauseNode(chatID, nodeID) {
			writeJSON(w, http.StatusConflict, schema.TransitionError{
				Error:   "node is not pausable right now (no live run); nothing was paused",
				Current: schema.NodeStatus(current),
				Allowed: allowedStatuses(current),
			})
			return
		}
		writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusPaused))
	case dag.StatusRunning:
		// paused → running: a fresh re-run reusing the plan's stored outputs.
		h.retryNodeAsync(dp, chatID, nodeID, guidance)
		writeJSON(w, http.StatusOK, schema.DagNodeState{Status: schema.NodeStatusQueued})
	case dag.StatusQueued:
		h.retryNodeAsync(dp, chatID, nodeID, guidance)
		writeJSON(w, http.StatusOK, schema.DagNodeState{Status: schema.NodeStatusQueued})
	}
}

// Appends a message to a running node's queue, delivered at its next turn boundary. 404s if the node isn't live.
func (h *Handler) QueueNodeMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.QueueMessageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	m, ok := h.orch.QueueNodeMessage(chatID, nodeID, strings.TrimSpace(body.Message))
	if !ok {
		http.Error(w, "node is not running right now; the message has nowhere to land", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, queuedMessageWire(m))
}

// Rewrites a not-yet-delivered queued message.
func (h *Handler) EditQueuedMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID, messageID schema.MessageID) {
	var body schema.QueueMessageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	if !h.orch.EditQueuedMessage(chatID, nodeID, messageID, strings.TrimSpace(body.Message)) {
		http.Error(w, "no such pending queued message (unknown, or already delivered)", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Drops a not-yet-delivered queued message.
func (h *Handler) RemoveQueuedMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID, messageID schema.MessageID) {
	if !h.orch.RemoveQueuedMessage(chatID, nodeID, messageID) {
		http.Error(w, "no such pending queued message (unknown, or already delivered)", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Replaces a not-yet-started node's task text. 409 once the node has started.
func (h *Handler) EditNodeTask(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.EditNodeTaskBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Task) == "" {
		http.Error(w, "task is required", http.StatusBadRequest)
		return
	}
	if !h.orch.SetNodeTaskOverride(chatID, nodeID, body.Task) {
		http.Error(w, "the node has already started; its prompt is immutable", http.StatusConflict)
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
	return ns
}

// allowedStatuses converts dag.AllowedTargets to the wire enum for a 409 body.
func allowedStatuses(from dag.NodeStatus) []schema.NodeStatus {
	targets := dag.AllowedTargets(from)
	out := make([]schema.NodeStatus, len(targets))
	for i, t := range targets {
		out[i] = schema.NodeStatus(t)
	}
	return out
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
		defer func() {
			cancelRun()
			h.hub.UnregisterRun(chatID)
		}()
		defer h.hub.Close(chatID)

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
		_ = h.store.Touch(runCtx, chatID)
	}()
}

// Connects a client to a chat's live (or just-completed) run. Reconnect-safe via Last-Event-ID or the durable event log.
func (h *Handler) SubscribeChatStream(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
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
	var msgItem schema.OutputItem
	if tc.AsstText != "" || tc.AsstThink != "" {
		content := make([]schema.ContentPart, 0, 2)
		if tc.AsstThink != "" {
			var cp schema.ContentPart
			_ = cp.FromReasoningPart(schema.ReasoningPart{Text: tc.AsstThink})
			content = append(content, cp)
		}
		if tc.AsstText != "" {
			var cp schema.ContentPart
			_ = cp.FromOutputTextPart(schema.OutputTextPart{Text: tc.AsstText})
			content = append(content, cp)
		}
		_ = msgItem.FromMessageOutputItem(schema.MessageOutputItem{
			Id:      tc.ID + ":msg",
			Status:  schema.Completed,
			Content: content,
		})
	}

	var dagItem *schema.OutputItem
	if tc.Plan != nil {
		var planData stream.DagPlanData
		if err := json.Unmarshal([]byte(tc.Plan.PlanJSON), &planData); err == nil {
			nodes := make([]schema.DagNodeDef, len(planData.Nodes))
			for i, n := range planData.Nodes {
				nodes[i] = schema.DagNodeDef{Id: n.ID, Agent: n.Agent, Task: n.Task, DependsOn: n.DependsOn}
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
				if ns.Status == schema.NodeStatusRunning || ns.Status == schema.NodeStatusQueued || ns.Status == schema.NodeStatusNeedsInput {
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
	if tc.AsstText != "" || tc.AsstThink != "" {
		output = append(output, msgItem)
	}

	// Orchestrator's token usage (UsageMetadata survives ADK's round-trip; ModelVersion doesn't, hence the separate model field).
	var usage *schema.Usage
	if tc.PromptTokens > 0 || tc.CompletionTokens > 0 || tc.ReasoningTokens > 0 {
		usage = &schema.Usage{
			InputTokens:  intPtr(int(tc.PromptTokens)),
			OutputTokens: intPtr(int(tc.CompletionTokens + tc.ReasoningTokens)),
		}
	}

	return schema.Turn{
		Id:        tc.ID,
		CreatedAt: tc.CreatedAt,
		Input:     schema.TurnInput{Role: schema.User, Content: tc.UserText},
		Output:    output,
		Usage:     usage,
		// Persisted on the turn row at run end (ADK drops ModelVersion). Nil for DAG turns.
		Model: strPtr(tc.Model),
	}
}

// Converts persisted store.DagNode into wire DagNodeState. Shared by buildTurn and UpdateNodeStatus.
func dagNodeState(n store.DagNode) schema.DagNodeState {
	ns := schema.DagNodeState{
		Status:           schema.NodeStatus(n.Status),
		Model:            strPtr(n.Model),
		FinishReason:     strPtr(n.FinishReason),
		OutputPreview:    strPtr(n.OutputPreview),
		Error:            strPtr(n.Error),
		PromptTokens:     intPtr(int(n.PromptTokens)),
		CompletionTokens: intPtr(int(n.CompletionTokens)),
		ReasoningTokens:  intPtr(int(n.ReasoningTokens)),
		TotalTokens:      intPtr(int(n.TotalTokens)),
		ServerDurationMs: intPtr(int(n.DurationMs)),
		JudgeRounds:      intPtr(int(n.JudgeRounds)),
		JudgeFinalScore:  float64Ptr(n.JudgeFinalScore),
		JudgePassed:      boolPtr(n.JudgePassed),
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

// Builds a ChatSummary with derived status. Loads turns itself (ponytail: batch only if list gets slow).
func (h *Handler) toSummary(ctx context.Context, c store.Chat) schema.ChatSummary {
	turns, _ := h.store.GetTurnsWithContent(ctx, orchestrator.AppName, store.SessionUserFor(c), c.ID)
	status, pendingQuestion := h.chatStatus(ctx, c.ID, turns)
	return schema.ChatSummary{
		Id:              c.ID,
		Title:           nonEmpty(c.Title),
		SystemPrompt:    c.SystemPrompt,
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
		Status:          status,
		PendingQuestion: pendingQuestion,
		GithubUrl:       nonEmpty(c.GithubURL),
		GithubRepo:      nonEmpty(c.GithubRepo),
	}
}

// Computes a chat's derived status: queued (waiting on max_active_runs), running (hub has a live run),
// needs_input (prior events end on an unanswered question), failed (last DAG node failed, no answer), or idle.
func (h *Handler) chatStatus(ctx context.Context, chatID string, turns []store.TurnContent) (schema.ChatStatus, *string) {
	if h.orch.Queued(chatID) {
		return schema.ChatStatusQueued, nil
	}
	if h.hub.Active(chatID) {
		return schema.ChatStatusRunning, nil
	}
	if pq, ok := orchestrator.LatestPendingQuestion(h.orch.PriorEvents(ctx, h.sessionUser(ctx, chatID), chatID)); ok {
		q := pq.Message
		return schema.ChatStatusNeedsInput, &q
	}
	if n := len(turns); n > 0 {
		last := turns[n-1]
		if strings.TrimSpace(last.AsstText) == "" && hasFailedNode(last.Nodes) {
			return schema.ChatStatusFailed, nil
		}
	}
	return schema.ChatStatusIdle, nil
}

func hasFailedNode(nodes []store.DagNode) bool {
	for _, n := range nodes {
		if n.Status == "failed" {
			return true
		}
	}
	return false
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, err error) {
	http.Error(w, err.Error(), status)
}

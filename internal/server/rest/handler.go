// Package rest implements the generated OpenAPI ServerInterface for Quack's
// REST surface: chat CRUD plus the streaming responses endpoint.
package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

const userID = "local"

const titleInstruction = "Generate a concise chat title (3–6 words, no punctuation, no quotes). Return only the title."

// runTimeout is the backstop that stops a wedged run leaking a goroutine
// forever — not a policy on how long work may take. It has to outlast the
// longest legitimate run (an overnight multi-node research/implement DAG), or it
// becomes the thing that kills long runs: the ceiling fires, every in-flight node
// dies with "emitUp: context canceled", and the chat goes idle with no answer.
const runTimeout = 24 * time.Hour

// activeRun is the live handle to a chat's in-flight run: its response id (the
// turn id, surfaced in the stream's opening response_created event) and the
// cancel func PUT .../responses/{response_id}/status invokes by id.
type activeRun struct {
	responseID string
	cancel     context.CancelFunc
}

// Handler implements schema.ServerInterface backed by the store + orchestrator.
type Handler struct {
	store         *store.Store
	orch          *orchestrator.Orchestrator
	titler        model.LLM
	jail          *workspace.Jail  // per-chat workspace tree cleanup on delete; nil ⇒ no workspace configured
	hub           *stream.Hub      // fans a chat's run to extra subscribers (other devices)
	eventLog      *runlog.EventLog // durably persists the run stream, backing replay across restarts
	activeCancels sync.Map         // chatID → *activeRun
}

// NewHandler builds a REST handler. jail may be nil (no workspace configured).
// hub may be nil to get a private hub; pass a shared *stream.Hub (e.g. from
// internal/serve) when another driver of runs on the same chats — such as the
// GitHub webhook dispatcher — needs live subscribers to see the same events.
func NewHandler(s *store.Store, o *orchestrator.Orchestrator, titler model.LLM, jail *workspace.Jail, hub *stream.Hub) *Handler {
	if hub == nil {
		hub = stream.NewHub()
	}
	return &Handler{store: s, orch: o, titler: titler, jail: jail, hub: hub, eventLog: runlog.NewEventLog(s)}
}

func (h *Handler) generateTitle(ctx context.Context, firstMessage string) string {
	if h.titler == nil {
		return ""
	}
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
	// Strip any leaked reasoning: /no_think above asks qwen to skip thinking, but
	// it sometimes emits a <think> block into content anyway (often UNCLOSED when
	// it runs to the token limit), which would otherwise become the "title".
	title := stream.StripThinking(out.String())
	slog.Info("title generated", "component", "title", "title", title, "candidates", candidates, "total", total)
	return title
}

func (h *Handler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) ListChats(w http.ResponseWriter, r *http.Request) {
	chats, err := h.store.ListChats(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	// Status computation reads each chat's session twice (turns + pending-question
	// scan) — serial, a ~15-chat list took 3-4s live. Fan out, keep order.
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
	turns, err := h.store.GetTurnsWithContent(r.Context(), orchestrator.AppName, userID, chatID)
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
	tc, err := h.store.GetTurnWithContent(r.Context(), orchestrator.AppName, userID, chatID, responseID)
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

func (h *Handler) DeleteChat(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	if err := h.store.DeleteChat(r.Context(), chatID); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	// Lifecycle cleanup: remove the chat's per-chat workspace tree
	// (<root>/<user>/<chatID>/) so a deleted chat doesn't leak its clones
	// forever. Best-effort — the chat row is already gone, and RemoveChatScope
	// sanitizes chatID (single path component) so the RemoveAll can't escape the
	// user root. A missing dir is a clean no-op; any error is logged and the
	// delete still succeeds. Skipped when no workspace is configured (jail nil).
	if h.jail != nil {
		if err := h.jail.RemoveChatScope(userID, chatID); err != nil {
			slog.Warn("per-chat workspace cleanup failed; chat deleted anyway",
				"component", "rest", "chat", chatID, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// SendChatMessage STARTS a run and streams it to this client as SSE. The run is
// server-side work with its own lifetime (see startRun); this request is just the
// first viewer of it. Accepts either application/json ({"content":"..."}) or
// multipart/form-data with a "content" text field and optional "files" file parts
// (image/audio).
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

	// Generate a stable turn ID before the run so the DAG plan can reference it,
	// and so it can be surfaced as this run's response_id (the very first event
	// of the stream) — the id a client names in PUT .../responses/{response_id}/status
	// to cancel this run.
	turnID := uuid.NewString()
	go func() { _ = h.store.SaveTurn(context.Background(), chatID, turnID) }()

	// Attach THIS client to the run's topic BEFORE the run starts publishing, so
	// it misses nothing (Reset drops the previous run's buffer first, exactly as
	// the run's first Publish would).
	h.hub.Reset(chatID)
	replay, live, unsubscribe, _ := h.hub.Subscribe(chatID)
	defer unsubscribe()

	h.startRun(chatID, turnID, body.Content, attachments)

	// From here this handler is only a VIEWER of the run: it forwards hub events
	// to this client and returns when the run ends or the client goes away. It
	// does not drive the run, so a slow, sleeping or vanished client can neither
	// stall it (SSE writes are off the run's critical path) nor kill it.
	streamHub(r.Context(), sse, replay, live, 0)
}

// startRun launches a chat's turn as server-side work: its own goroutine on a
// context tied to the SERVER's lifetime (bounded by runTimeout) plus the
// explicit-cancel path — NOT to the HTTP request that kicked it off. A run
// outlives its initiating client: a dropped curl, a closed tab or a laptop going
// to sleep stops the client READING the stream; the DAG keeps executing, keeps
// publishing to the hub + durable event log, and any client (this one on
// reconnect, or another device via GET .../stream) can attach and watch it live.
// Only PUT .../responses/{id}/status {"status":"cancelled"} may kill it.
func (h *Handler) startRun(chatID, turnID, content string, attachments []*genai.Part) {
	runCtx, cancelRun := context.WithTimeout(context.Background(), runTimeout)
	// Registered synchronously (before the goroutine gets to run) so the cancel
	// endpoint can never miss the run it was just told about.
	h.activeCancels.Store(chatID, &activeRun{responseID: turnID, cancel: cancelRun})
	go func() {
		// Mark the run finished for hub subscribers once it returns (after its final
		// Done is published) — LAST, so that by the time a viewer sees the stream
		// close, the run is already off activeCancels (cancelling it now 404s).
		defer h.hub.Close(chatID)
		defer func() {
			cancelRun()
			h.activeCancels.Delete(chatID)
		}()
		h.runChat(runCtx, chatID, turnID, content, attachments)
	}()
}

// runChat is the run itself: it drives the orchestrator and publishes the whole
// SSE stream to the hub (live subscribers) and the durable event log. It writes
// to no HTTP client — a run has no client, only viewers.
func (h *Handler) runChat(runCtx context.Context, chatID, turnID, message string, attachments []*genai.Part) {
	// Fresh run: clear the previous run's durable events so this run's seq starts
	// at 1 (mirrors the hub topic reset on the way in).
	h.eventLog.Reset(runCtx, chatID)

	// pub assigns the next per-chat seq, fans the event to live hub subscribers,
	// and persists it durably (off the hot path). The run loop is the sole publisher
	// for this chat, so seq stays monotonic without locking.
	pub := runlog.NewPublisher(h.hub, h.eventLog, chatID)
	publish := pub.Publish

	// response_created is always the very first event of the stream.
	publish(stream.ResponseCreated(turnID))

	titleCh := make(chan string, 1)
	go func() {
		defer close(titleCh)
		c, _ := h.store.GetChat(runCtx, chatID)
		if c == nil || c.Title != "" {
			return
		}
		title := h.generateTitle(runCtx, message)
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
	// The model behind the orchestrator's own reply, captured from its top-level
	// agent_complete (empty node_id). Stamped onto the turn row after the run —
	// ADK's event storage drops ModelVersion, so history can't recover it later.
	var orchModel string

	for ev, err := range h.orch.Run(runCtx, userID, chatID, message, attachments) {
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
	// Stamp the orchestrator's model on the turn row so history attribution
	// matches the live stream. Plain replies only — a DAG turn's models live
	// per-node on DagNodeState, and its answer is credited to the terminal node.
	if orchModel != "" && activePlanID == "" {
		_ = h.store.SetTurnModel(runCtx, chatID, turnID, orchModel)
	}
	_ = h.store.Touch(runCtx, chatID)
}

// UpdateResponseStatus cancels the chat's active run — but only when
// response_id names that run (the id surfaced in its opening response_created
// event); a stale or already-finished id 404s rather than silently no-op'ing.
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
	v, ok := h.activeCancels.Load(chatID)
	run, _ := v.(*activeRun)
	if !ok || run == nil || run.responseID != responseID {
		http.Error(w, "no active run with this response id", http.StatusNotFound)
		return
	}
	run.cancel()
	w.WriteHeader(http.StatusNoContent)
}

// UpdateNodeStatus transitions one DAG node's status — replacing the old
// cancel/steer/retry RPC verbs with a single resource-oriented PUT naming the
// TARGET status. Every write routes through dag.CanTransition: an illegal
// transition from the node's current (persisted) status 409s naming the legal
// targets.
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

	// The plan itself is loaded from session state by the orchestrator (the real
	// dag.Plan) when it actually runs. Here we just need the plan/node defs (to
	// confirm the node exists) and the persisted outputs a retry reuses.
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

	if target == dag.StatusRunning && guidance == "" {
		http.Error(w, "guidance is required to steer a running node", http.StatusBadRequest)
		return
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
		// Delivery is NOT optimistic (same as steer below): CancelNode returns
		// false when no live control is registered for the node — the signal
		// landed nowhere, and answering 200 + "cancelled" anyway makes the API lie.
		//
		// A delivered cancel is still COOPERATIVE: the node's next tool call
		// fails fast (tools.Deps.NodeCancelled) and the gate stops it at its
		// next stage boundary, keeping its partial answer (continue-but-warn).
		// So 200 means "the running node has been told", not "it has stopped";
		// the stop is durably reflected by the node_cancelled SSE event.
		if !h.orch.CancelNode(chatID, nodeID) {
			writeJSON(w, http.StatusConflict, schema.TransitionError{
				Error:   "node is not cancellable right now (no live run — it may be queued but not yet dispatched, or already finished); nothing was cancelled",
				Current: schema.NodeStatus(current),
				Allowed: allowedStatuses(current),
			})
			return
		}
		writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusCancelled))
	case dag.StatusRunning:
		// Steer delivery is NOT optimistic: unlike cancel (whose no-op case is
		// benign), a silently dropped steer looks like "steer doesn't work" —
		// the signal only lands while the node has a live control, and a node
		// that is mid-restart from a PREVIOUS steer has none. Tell the client.
		if !h.orch.SteerNode(chatID, nodeID, guidance) {
			writeJSON(w, http.StatusConflict, schema.TransitionError{
				Error:   "node is not steerable right now (no live run — it may be restarting from an earlier steer or finishing); try again in a moment",
				Current: schema.NodeStatus(current),
				Allowed: allowedStatuses(current),
			})
			return
		}
		writeJSON(w, http.StatusOK, optimisticNodeState(dn, dag.StatusRunning))
	case dag.StatusQueued:
		h.retryNodeAsync(dp, chatID, nodeID, guidance)
		// The re-run is optimistically queued; its progress streams over the
		// chat's existing hub/event-log relay (GET .../stream), same as any run.
		writeJSON(w, http.StatusOK, schema.DagNodeState{Status: schema.NodeStatusQueued})
	}
}

// optimisticNodeState builds a PUT node-status response body from the node's
// persisted row (nil when it has no row yet — implicitly dag.StatusQueued),
// with Status overridden to the just-accepted target — the transition was
// legal and accepted even though propagation (cancel/steer) is cooperative and
// completes asynchronously via the SSE stream.
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

// retryNodeAsync re-runs nodeID and its descendants in the background, reusing
// the rest of the plan's stored outputs. It does not stream its own HTTP
// response (the PUT that triggered it already returned) — progress publishes
// through the same hub + durable event log a GET .../stream subscriber already
// watches, exactly like a fresh run.
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
		h.activeCancels.Store(chatID, &activeRun{responseID: dp.TurnID, cancel: cancelRun})
		defer func() {
			cancelRun()
			h.activeCancels.Delete(chatID)
		}()
		defer h.hub.Close(chatID)

		h.eventLog.Reset(runCtx, chatID)
		publish := runlog.NewPublisher(h.hub, h.eventLog, chatID).Publish
		publish(stream.ResponseCreated(dp.TurnID))

		for ev, err := range h.orch.RetryNode(runCtx, userID, chatID, seeded, nodeID, guidance) {
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

// SubscribeChatStream connects an additional client to a chat's live (or
// just-completed) run: it replays the events so far, then streams live events
// until the run ends — so a turn started on one device can be watched from
// another, or resumed by the same browser after a reload. Reconnect-safe two
// ways: the hub's in-memory buffer on the warm path, and the durable event log
// (store.ChatEvent) when the hub is cold (after a server restart). A client
// resumes mid-stream by sending its last-seen seq via Last-Event-ID (the SSE id
// emitted on every event) or the last_event_id query param.
func (h *Handler) SubscribeChatStream(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	lastSeq := lastEventID(r)
	replay, live, cancel, done := h.hub.Subscribe(chatID)
	defer cancel()
	_, active := h.activeCancels.Load(chatID)

	// Cold path: the hub has no buffered events and no run is active in this
	// process — a restart wiped the in-memory topic (the orchestrator goroutine
	// died too). Replay the run from the durable log; there is nothing live to
	// tail (a new run would repopulate the hub, taking the warm path below).
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

	// Warm path: the hub holds the run (live, or completed-but-still-buffered).
	// Replay its buffer (skipping anything the client already saw), then tail live.
	if done { // run already finished; replay holds the whole stream (live is nil)
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

// streamHub forwards one run's events to one client: the replay buffer, then the
// live tail, until the run ends (the hub closes the channel) or the client goes
// away (its request context is done, or a write fails). It is the ONLY place a
// run's events touch an HTTP connection — the run itself publishes to the hub and
// never blocks on a client, so a viewer that is slow, asleep or gone cannot stall
// or kill it. lastSeq skips whatever the client already saw.
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

// lastEventID reads a reconnecting subscriber's last-seen seq from the standard
// Last-Event-ID header (set automatically by EventSource on reconnect) or the
// last_event_id query param. 0 (no/invalid value) replays from the start.
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

// buildTurn converts a TurnContent (store layer) into a schema.Turn (API layer).
func buildTurn(tc store.TurnContent) schema.Turn {
	// Message output item — always present when there is assistant text.
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

	// DAG output item — present when a plan was executed this turn.
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
			// DAG is completed if all nodes are done/failed/cancelled, in_progress otherwise.
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

	// Agent-activity output item — the orchestrator's own tool calls (plan, execute,
	// get_user_choice), recovered from session events so chat history (incl. a
	// pending clarification) survives a reload.
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
	// Only append message item if it has content.
	if tc.AsstText != "" || tc.AsstThink != "" {
		output = append(output, msgItem)
	}

	// Usage: the orchestrator's own token usage for this turn (recovered from the
	// stored session events — UsageMetadata survives ADK's Postgres round-trip,
	// unlike ModelVersion, which the storage layer drops; that's why Model below
	// comes from our own turn row instead). Nil when nothing was recorded (e.g. a
	// turn that only ran a DAG, whose per-node tokens live on DagNodeState).
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
		// The orchestrator's own model, persisted on the turn row at run end
		// (ADK's event storage drops ModelVersion, so it's not recoverable from
		// session events). Nil for DAG turns.
		Model: strPtr(tc.Model),
	}
}

// dagNodeState converts a store.DagNode (persisted) into the wire DagNodeState.
// Shared by buildTurn (chat history) and UpdateNodeStatus (the PUT response).
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

// toSummary builds a ChatSummary, including its derived status. Loads the
// chat's turns itself (an acceptable per-chat query in chat list — ponytail:
// only worth batching if list gets slow); GetChat already has its turns loaded
// and calls chatStatus directly instead.
func (h *Handler) toSummary(ctx context.Context, c store.Chat) schema.ChatSummary {
	turns, _ := h.store.GetTurnsWithContent(ctx, orchestrator.AppName, userID, c.ID)
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

// chatStatus computes a chat's derived status plus its pending question (set
// only for needs_input):
//
//   - running — the hub has a live run for this chat.
//   - needs_input — the chat's session history ends on an unanswered question.
//     This MUST be (and is) the same scan the orchestrator's own resume path
//     uses (orchestrator.LatestPendingQuestion over PriorEvents) — one place
//     decides "is a question pending", so the API and the resume behavior can
//     never disagree (see AGENTS.md's DRY requirement for chat status).
//   - failed — the last turn's DAG has a failed node and no answer text
//     followed it.
//   - idle — none of the above.
func (h *Handler) chatStatus(ctx context.Context, chatID string, turns []store.TurnContent) (schema.ChatStatus, *string) {
	if h.hub.Active(chatID) {
		return schema.ChatStatusRunning, nil
	}
	if pq, ok := orchestrator.LatestPendingQuestion(h.orch.PriorEvents(ctx, userID, chatID)); ok {
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

// hasFailedNode reports whether any node in a turn's DAG failed.
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

// Package rest implements the generated OpenAPI ServerInterface for Quack's
// REST surface: chat CRUD plus the streaming responses endpoint.
package rest

import (
	"context"
	"encoding/json"
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
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

const userID = "local"

const titleInstruction = "Generate a concise chat title (3–6 words, no punctuation, no quotes). Return only the title."

const runTimeout = 2 * time.Hour

// Handler implements schema.ServerInterface backed by the store + orchestrator.
type Handler struct {
	store         *store.Store
	orch          *orchestrator.Orchestrator
	titler        model.LLM
	hub           *stream.Hub // fans a chat's run to extra subscribers (other devices)
	eventLog      *eventLog   // durably persists the run stream, backing replay across restarts
	activeCancels sync.Map    // chatID → context.CancelFunc
}

// NewHandler builds a REST handler.
func NewHandler(s *store.Store, o *orchestrator.Orchestrator, titler model.LLM) *Handler {
	return &Handler{store: s, orch: o, titler: titler, hub: stream.NewHub(), eventLog: newEventLog(s)}
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
	out := schema.ChatList{Data: make([]schema.ChatSummary, 0, len(chats))}
	for _, c := range chats {
		out.Data = append(out.Data, toSummary(c))
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
	writeJSON(w, http.StatusOK, toSummary(*c))
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
	detail := schema.ChatDetail{
		Id:           c.ID,
		Title:        nonEmpty(c.Title),
		SystemPrompt: c.SystemPrompt,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
		Turns:        make([]schema.Turn, 0, len(turns)),
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
	w.WriteHeader(http.StatusNoContent)
}

// SendChatMessage runs the orchestrator and streams the response as SSE.
// Accepts either application/json ({"content":"..."}) or multipart/form-data
// with a "content" text field and optional "files" file parts (image/audio).
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

	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(r.Context()), runTimeout)
	h.activeCancels.Store(chatID, cancelRun)
	defer func() {
		cancelRun()
		h.activeCancels.Delete(chatID)
	}()
	// Mark the run finished for hub subscribers once this returns (after the final
	// Done is published below). The run uses a detached context, so it — and the
	// hub fan-out — continue even if this initiating client disconnects.
	defer h.hub.Close(chatID)

	// Fresh run: clear the previous run's durable events so this run's seq starts
	// at 1 (mirrors the hub discarding the old buffer on its first publish).
	h.eventLog.reset(runCtx, chatID)

	// publish assigns the next per-chat seq, fans the event to live hub subscribers,
	// and persists it durably (off the hot path). The run loop is the sole publisher
	// for this chat, so seq stays monotonic without locking.
	var seq int64
	publish := func(ev stream.SSEEvent) {
		seq++
		h.hub.Publish(chatID, seq, ev)
		h.eventLog.append(chatID, seq, ev)
	}

	// Generate a stable turn ID before the run so the DAG plan can reference it.
	turnID := uuid.NewString()
	go func() { _ = h.store.SaveTurn(context.Background(), chatID, turnID) }()

	titleCh := make(chan string, 1)
	go func() {
		defer close(titleCh)
		c, _ := h.store.GetChat(runCtx, chatID)
		if c == nil || c.Title != "" {
			return
		}
		title := h.generateTitle(runCtx, body.Content)
		if title == "" {
			return
		}
		_ = h.store.UpdateTitle(runCtx, chatID, title)
		titleCh <- title
	}()

	clientGone := false
	trySendTitle := func() {
		select {
		case title, ok := <-titleCh:
			if ok {
				publish(stream.ChatTitle(title))
				if !clientGone {
					_ = sse.send(stream.ChatTitle(title))
				}
			}
		default:
		}
	}

	var activePlanID string

	for ev, err := range h.orch.Run(runCtx, userID, chatID, body.Content, attachments) {
		trySendTitle()
		if err != nil {
			publish(stream.Errorf(err.Error()))
			publish(stream.Done())
			if !clientGone {
				_ = sse.send(stream.Errorf(err.Error()))
				_ = sse.send(stream.Done())
			}
			return
		}
		if ev.Name == stream.EventDagPlan {
			if d, ok := ev.Data.(stream.DagPlanData); ok {
				activePlanID = d.PlanID
				planJSON, _ := json.Marshal(d)
				go func() {
					_ = h.store.SaveDagPlan(context.Background(), chatID, d.PlanID, turnID, string(planJSON))
				}()
			}
		} else if activePlanID != "" {
			h.persistNodeEvent(activePlanID, ev)
		}
		// Fan out to any other subscribers regardless of whether THIS client is
		// still connected; only the direct write is gated on clientGone.
		publish(ev)
		if !clientGone {
			if sendErr := sse.send(ev); sendErr != nil {
				clientGone = true
			}
		}
	}
	for title := range titleCh {
		publish(stream.ChatTitle(title))
		if !clientGone {
			_ = sse.send(stream.ChatTitle(title))
		}
	}
	publish(stream.Done())
	if !clientGone {
		_ = sse.send(stream.Done())
	}
	_ = h.store.Touch(runCtx, chatID)
}

func (h *Handler) CancelChatStream(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	if cancel, ok := h.activeCancels.Load(chatID); ok {
		cancel.(context.CancelFunc)()
	}
	w.WriteHeader(http.StatusNoContent)
}

// CancelNode stops a single running node of the chat's active run; the rest of
// the DAG keeps going (continue-but-warn). No-op if no such node is active.
func (h *Handler) CancelNode(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	h.orch.CancelNode(chatID, nodeID)
	w.WriteHeader(http.StatusNoContent)
}

// SteerNode interrupts a single running node and re-runs it with new guidance
// against its same session (prior tool calls/results retained). No-op if no such
// node is active. The re-run streams over the chat's existing SSE connection.
func (h *Handler) SteerNode(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.SteerNodeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Guidance) == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	h.orch.SteerNode(chatID, nodeID, body.Guidance)
	w.WriteHeader(http.StatusNoContent)
}

// RetryNode re-runs a FINISHED node (failed or done) and its descendants, reusing
// the stored outputs of every other node, and streams the re-execution as SSE.
// Optional guidance is folded into the node's task. The new node states are
// persisted (DagNode store) and the new terminal answer replaces the chat's answer.
func (h *Handler) RetryNode(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	var body schema.RetryNodeBody
	_ = json.NewDecoder(r.Body).Decode(&body) // guidance is optional; empty body is fine
	guidance := ""
	if body.Guidance != nil {
		guidance = *body.Guidance
	}

	// Load the chat's latest plan and the stored node outputs to reuse.
	dp, err := h.store.GetLatestDagPlan(r.Context(), chatID)
	if err != nil || dp == nil {
		http.Error(w, "no plan to retry for this chat", http.StatusNotFound)
		return
	}
	var plan dag.Plan
	if err := json.Unmarshal([]byte(dp.PlanJSON), &plan); err != nil {
		http.Error(w, "stored plan is corrupt", http.StatusInternalServerError)
		return
	}
	found := false
	for _, n := range plan.Nodes {
		if n.ID == nodeID {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "no such node in the plan", http.StatusNotFound)
		return
	}
	nodes, _ := h.store.GetDagNodes(r.Context(), plan.ID)
	seeded := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.Output != "" {
			seeded[n.NodeID] = n.Output
		}
	}

	sse, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(r.Context()), runTimeout)
	h.activeCancels.Store(chatID, cancelRun)
	defer func() {
		cancelRun()
		h.activeCancels.Delete(chatID)
	}()

	for ev, err := range h.orch.RetryNode(runCtx, userID, chatID, plan, seeded, nodeID, guidance) {
		if err != nil {
			_ = sse.send(stream.Errorf(err.Error()))
			break
		}
		h.persistNodeEvent(plan.ID, ev) // update the re-run nodes' persisted state
		if sendErr := sse.send(ev); sendErr != nil {
			break
		}
	}
	_ = sse.send(stream.Done())
	_ = h.store.Touch(runCtx, chatID)
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
			ev, err := unmarshalEvent(e.Event)
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
	for _, it := range replay {
		if it.Seq <= lastSeq {
			continue
		}
		if sse.sendID(it.Seq, it.SSE) != nil {
			return
		}
	}
	if done {
		return // run already finished; replay included its terminal Done event
	}
	for {
		select {
		case it, ok := <-live:
			if !ok {
				return // run ended (its Done was delivered via the live channel)
			}
			if it.Seq <= lastSeq {
				continue
			}
			if sse.sendID(it.Seq, it.SSE) != nil {
				return
			}
		case <-r.Context().Done():
			return
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
				ns := schema.DagNodeState{
					Status:           n.Status,
					Model:            strPtr(n.Model),
					FinishReason:     strPtr(n.FinishReason),
					OutputPreview:    strPtr(n.OutputPreview),
					Error:            strPtr(n.Error),
					PromptTokens:     intPtr(int(n.PromptTokens)),
					CompletionTokens: intPtr(int(n.CompletionTokens)),
					ReasoningTokens:  intPtr(int(n.ReasoningTokens)),
					TotalTokens:      intPtr(int(n.TotalTokens)),
					ServerDurationMs: intPtr(int(n.DurationMs)),
					SelfRefined:      boolPtr(n.SelfRefined),
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
				nodeStates[n.NodeID] = ns
			}
			// DAG is completed if all nodes are done/failed, in_progress otherwise.
			dagStatus := schema.Completed
			for _, ns := range nodeStates {
				if ns.Status == "running" || ns.Status == "queued" {
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

	return schema.Turn{
		Id:        tc.ID,
		CreatedAt: tc.CreatedAt,
		Input:     schema.TurnInput{Role: schema.User, Content: tc.UserText},
		Output:    output,
	}
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

func toSummary(c store.Chat) schema.ChatSummary {
	return schema.ChatSummary{
		Id:           c.ID,
		Title:        nonEmpty(c.Title),
		SystemPrompt: c.SystemPrompt,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
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

// persistNodeEvent upserts the persisted DagNode state for a node-lifecycle event
// (running / done / failed). Ignores non-node events.
func (h *Handler) persistNodeEvent(planID string, ev stream.SSEEvent) {
	t := time.Now().UTC()
	up := func(n store.DagNode) { go func() { _ = h.store.UpsertDagNode(context.Background(), n) }() }
	switch d := ev.Data.(type) {
	case stream.NodeStartData:
		up(store.DagNode{NodeID: d.NodeID, PlanID: planID, Status: "running", StartedAt: &t})
	case stream.NodeDoneData:
		up(store.DagNode{
			NodeID: d.NodeID, PlanID: planID, Status: "done",
			OutputPreview: d.OutputPreview, Output: d.Output, FinishedAt: &t,
			Model: d.Model, PromptTokens: d.PromptTokens, CompletionTokens: d.CompletionTokens,
			ReasoningTokens: d.ReasoningTokens, TotalTokens: d.TotalTokens,
			FinishReason: d.FinishReason, DurationMs: d.DurationMs, SelfRefined: d.SelfRefined,
			JudgeRounds: d.JudgeRounds, JudgeFinalScore: d.JudgeFinalScore, JudgePassed: d.JudgePassed,
		})
	case stream.NodeFailedData:
		up(store.DagNode{NodeID: d.NodeID, PlanID: planID, Status: "failed", Error: d.Error, FinishedAt: &t})
	}
}

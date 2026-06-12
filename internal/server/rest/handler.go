// Package rest implements the generated OpenAPI ServerInterface for Quack's
// REST surface: chat CRUD plus the streaming responses endpoint.
package rest

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/model"
	"google.golang.org/genai"

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
	activeCancels sync.Map // chatID → context.CancelFunc
}

// NewHandler builds a REST handler.
func NewHandler(s *store.Store, o *orchestrator.Orchestrator, titler model.LLM) *Handler {
	return &Handler{store: s, orch: o, titler: titler}
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
			log.Printf("title: generation failed: %v", err)
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
	title := strings.TrimSpace(out.String())
	log.Printf("title: %q (candidates=%d total=%d)", title, candidates, total)
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
func (h *Handler) SendChatMessage(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	var body schema.SendMessageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == "" {
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
			if ok && !clientGone {
				_ = sse.send(stream.ChatTitle(title))
			}
		default:
		}
	}

	var activePlanID string
	now := func() *time.Time { t := time.Now().UTC(); return &t }

	for ev, err := range h.orch.Run(runCtx, userID, chatID, body.Content) {
		trySendTitle()
		if err != nil {
			if !clientGone {
				_ = sse.send(stream.Errorf(err.Error()))
				_ = sse.send(stream.Done())
			}
			return
		}
		switch ev.Name {
		case stream.EventDagPlan:
			if d, ok := ev.Data.(stream.DagPlanData); ok {
				activePlanID = d.PlanID
				planJSON, _ := json.Marshal(d)
				go func() {
					_ = h.store.SaveDagPlan(context.Background(), chatID, d.PlanID, turnID, string(planJSON))
				}()
			}
		case stream.EventNodeStart:
			if d, ok := ev.Data.(stream.NodeStartData); ok && activePlanID != "" {
				pid := activePlanID
				go func() {
					_ = h.store.UpsertDagNode(context.Background(), store.DagNode{
						NodeID: d.NodeID, PlanID: pid, Status: "running", StartedAt: now(),
					})
				}()
			}
		case stream.EventNodeDone:
			if d, ok := ev.Data.(stream.NodeDoneData); ok && activePlanID != "" {
				pid := activePlanID
				go func() {
					_ = h.store.UpsertDagNode(context.Background(), store.DagNode{
						NodeID:           d.NodeID,
						PlanID:           pid,
						Status:           "done",
						OutputPreview:    d.OutputPreview,
						FinishedAt:       now(),
						Model:            d.Model,
						PromptTokens:     d.PromptTokens,
						CompletionTokens: d.CompletionTokens,
						ReasoningTokens:  d.ReasoningTokens,
						TotalTokens:      d.TotalTokens,
						FinishReason:     d.FinishReason,
						DurationMs:       d.DurationMs,
						SelfRefined:      d.SelfRefined,
						JudgeRounds:      d.JudgeRounds,
						JudgeFinalScore:  d.JudgeFinalScore,
						JudgePassed:      d.JudgePassed,
					})
				}()
			}
		case stream.EventNodeFailed:
			if d, ok := ev.Data.(stream.NodeFailedData); ok && activePlanID != "" {
				pid := activePlanID
				go func() {
					_ = h.store.UpsertDagNode(context.Background(), store.DagNode{
						NodeID: d.NodeID, PlanID: pid, Status: "failed", Error: d.Error, FinishedAt: now(),
					})
				}()
			}
		}
		if clientGone {
			continue
		}
		if sendErr := sse.send(ev); sendErr != nil {
			clientGone = true
		}
	}
	for title := range titleCh {
		if !clientGone {
			_ = sse.send(stream.ChatTitle(title))
		}
	}
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

	output := make([]schema.OutputItem, 0, 2)
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

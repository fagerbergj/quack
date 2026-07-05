package rest

import (
	"context"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
)

// TestChatStatusIdle: a fresh chat with no turns, no session activity, and no
// live run is idle.
func TestChatStatusIdle(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	got := h.toSummary(ctx, *c)
	if got.Status != schema.ChatStatusIdle {
		t.Errorf("status = %q, want idle", got.Status)
	}
	if got.PendingQuestion != nil {
		t.Errorf("pending_question = %v, want nil", got.PendingQuestion)
	}
}

// TestChatStatusRunning: the hub having a live topic for the chat wins over
// everything else (checked first).
func TestChatStatusRunning(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	h.hub.Publish(c.ID, 1, stream.SSEEvent{Name: "node_start"})

	got := h.toSummary(ctx, *c)
	if got.Status != schema.ChatStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}

// TestChatStatusNeedsInput: a pending get_user_choice clarification in the
// chat's session — the SAME scan Run's resume dispatch uses
// (orchestrator.LatestPendingQuestion) — surfaces as needs_input with the
// question text.
func TestChatStatusNeedsInput(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	const callID = "call-1"
	resp, err := h.store.Sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: c.ID})
	if err != nil {
		t.Fatalf("session Create: %v", err)
	}
	ask := session.NewEvent(ctx, "test")
	ask.Author = "orchestrator"
	ask.LongRunningToolIDs = []string{callID}
	ask.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{ID: callID, Name: tools.ChoiceToolName, Args: map[string]any{
			"question": "which Springfield?", "options": []string{"Illinois", "Missouri"},
		}},
	}}}
	if err := h.store.Sessions.AppendEvent(ctx, resp.Session, ask); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	placeholder := session.NewEvent(ctx, "test")
	placeholder.Author = "user"
	placeholder.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: callID, Name: tools.ChoiceToolName, Response: map[string]any{"status": "pending"}},
	}}}
	if err := h.store.Sessions.AppendEvent(ctx, resp.Session, placeholder); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	got := h.toSummary(ctx, *c)
	if got.Status != schema.ChatStatusNeedsInput {
		t.Fatalf("status = %q, want needs_input", got.Status)
	}
	if got.PendingQuestion == nil || *got.PendingQuestion != "which Springfield?" {
		t.Errorf("pending_question = %v, want %q", got.PendingQuestion, "which Springfield?")
	}
}

// TestChatStatusFailed: the last turn's DAG has a failed node and no assistant
// text followed it.
func TestChatStatusFailed(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := h.store.SaveTurn(ctx, c.ID, "t1"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := h.store.SaveDagPlan(ctx, c.ID, "p1", "t1", `{"nodes":[]}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := h.store.UpsertDagNode(ctx, store.DagNode{NodeID: "n1", PlanID: "p1", Status: "failed", Error: "boom"}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	got := h.toSummary(ctx, *c)
	if got.Status != schema.ChatStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// TestBuildTurnUsage covers PR2 item 2: buildTurn must populate Turn.usage from
// the orchestrator's own accumulated token counts (store.TurnContent, itself
// summed from stored ADK session events — see store.groupSessionEvents).
// input_tokens = prompt; output_tokens folds candidates + reasoning together
// (schema.Usage has no separate reasoning field).
func TestBuildTurnUsage(t *testing.T) {
	tc := store.TurnContent{
		ID:               "t1",
		CreatedAt:        time.Now(),
		UserText:         "what is the tallest mountain?",
		AsstText:         "Mount Everest.",
		PromptTokens:     40,
		CompletionTokens: 15,
		ReasoningTokens:  2,
		Model:            "gpt-oss-120b",
	}

	turn := buildTurn(tc)

	if turn.Usage == nil {
		t.Fatal("Usage = nil, want populated")
	}
	if turn.Usage.InputTokens == nil || *turn.Usage.InputTokens != 40 {
		t.Errorf("InputTokens = %v, want 40", turn.Usage.InputTokens)
	}
	if turn.Usage.OutputTokens == nil || *turn.Usage.OutputTokens != 17 {
		t.Errorf("OutputTokens = %v, want 17 (completion + reasoning)", turn.Usage.OutputTokens)
	}
	if turn.Model == nil || *turn.Model != "gpt-oss-120b" {
		t.Errorf("Model = %v, want gpt-oss-120b (from the persisted turn row)", turn.Model)
	}
}

// TestBuildTurnUsageNilWhenAbsent covers a DAG-only turn: the orchestrator itself
// recorded no tokens (all the work happened in gated nodes, surfaced separately
// via DagNodeState) — Turn.usage must stay nil, not a zero-valued struct, so the
// frontend can tell "no data" from "genuinely zero usage".
func TestBuildTurnUsageNilWhenAbsent(t *testing.T) {
	tc := store.TurnContent{ID: "t2", CreatedAt: time.Now(), UserText: "research X", AsstText: "The vetted answer."}

	turn := buildTurn(tc)

	if turn.Usage != nil {
		t.Errorf("Usage = %+v, want nil", turn.Usage)
	}
	if turn.Model != nil {
		t.Errorf("Model = %v, want nil (DAG turns carry no orchestrator model)", turn.Model)
	}
}

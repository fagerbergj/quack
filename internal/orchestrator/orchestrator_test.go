package orchestrator

import (
	"context"
	"testing"

	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
)

// appendUserText / appendModelText add a turn-shaped event to a session.
func appendEvent(t *testing.T, svc session.Service, sess session.Session, author, text string, thought bool) {
	t.Helper()
	ev := session.NewEvent("test")
	ev.Author = author
	role := "user"
	if author != "user" {
		role = "model"
	}
	ev.Content = &genai.Content{Role: role, Parts: []*genai.Part{{Text: text, Thought: thought}}}
	if err := svc.AppendEvent(context.Background(), sess, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// TestBuildHistory verifies prior turns are returned in order, assistant
// thinking is dropped, and a half-finished turn (user with no assistant reply,
// e.g. an unanswered clarifying question or a paused DAG) still contributes its
// user line so the planner sees the open question.
func TestBuildHistory(t *testing.T) {
	svc := session.InMemoryService()
	ctx := context.Background()
	const userID, sessionID = "u1", "c1"
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: AppName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess := resp.Session

	// Turn 1: complete exchange. Turn 2: user-only (half-finished).
	appendEvent(t, svc, sess, "user", "which Springfield?", false)
	appendEvent(t, svc, sess, "orchestrator", "let me think", true) // thinking — dropped
	appendEvent(t, svc, sess, "orchestrator", "Illinois or Missouri?", false)
	appendEvent(t, svc, sess, "user", "Illinois", false)

	o := &Orchestrator{sessions: svc}
	got := buildHistory(o.priorEvents(ctx, userID, sessionID))

	want := []dag.HistoryTurn{
		{Role: "user", Text: "which Springfield?"},
		{Role: "model", Text: "Illinois or Missouri?"},
		{Role: "user", Text: "Illinois"}, // half-finished turn: user line only
	}
	if len(got) != len(want) {
		t.Fatalf("buildHistory returned %d turns, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBuildHistoryEmptySession returns nil for a session that doesn't exist.
func TestBuildHistoryEmptySession(t *testing.T) {
	o := &Orchestrator{sessions: session.InMemoryService()}
	if got := buildHistory(o.priorEvents(context.Background(), "u", "missing")); got != nil {
		t.Errorf("buildHistory on missing session = %+v, want nil", got)
	}
}

// appendPartsEvent appends a model-authored event carrying arbitrary parts.
func appendPartsEvent(t *testing.T, svc session.Service, sess session.Session, longRunningIDs []string, parts ...*genai.Part) {
	t.Helper()
	ev := session.NewEvent("test")
	ev.Author = "orchestrator"
	ev.LongRunningToolIDs = longRunningIDs
	ev.Content = &genai.Content{Role: "model", Parts: parts}
	if err := svc.AppendEvent(context.Background(), sess, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// TestPendingChoiceCallID verifies a pending get_user_choice call is detected
// while unanswered and clears once a real answer (carrying the answer key)
// follows — so the orchestrator resumes the right turn exactly once.
func TestPendingChoiceCallID(t *testing.T) {
	svc := session.InMemoryService()
	ctx := context.Background()
	const userID, sessionID, callID = "u1", "c1", "call-xyz"
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: AppName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess := resp.Session
	o := &Orchestrator{sessions: svc}

	// No clarification yet → nothing pending.
	if got := pendingChoiceCallID(o.priorEvents(ctx, userID, sessionID)); got != "" {
		t.Errorf("fresh session pendingChoiceCallID = %q, want empty", got)
	}

	// The orchestrator asks: a long-running choice call + its auto pending
	// placeholder response (note: NO answer key).
	appendPartsEvent(t, svc, sess, []string{callID},
		&genai.Part{FunctionCall: &genai.FunctionCall{ID: callID, Name: tools.ChoiceToolName, Args: map[string]any{"options": []string{"Illinois", "Missouri"}}}})
	appendPartsEvent(t, svc, sess, nil,
		&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: callID, Name: tools.ChoiceToolName, Response: map[string]any{"status": "pending"}}})

	if got := pendingChoiceCallID(o.priorEvents(ctx, userID, sessionID)); got != callID {
		t.Errorf("after ask, pendingChoiceCallID = %q, want %q", got, callID)
	}

	// The user answers: a FunctionResponse carrying the answer key resolves it.
	appendPartsEvent(t, svc, sess, nil,
		&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: callID, Name: tools.ChoiceToolName, Response: map[string]any{tools.ChoiceAnswerKey: "Illinois"}}})

	if got := pendingChoiceCallID(o.priorEvents(ctx, userID, sessionID)); got != "" {
		t.Errorf("after answer, pendingChoiceCallID = %q, want empty", got)
	}
}

// TestOrchestratorReturnType is a compile-time check that Run returns SSEEvent,
// not *session.Event. A later integration test in internal/agent covers the full
// A2A round trip; this package test only covers the orchestrator's own logic.
func TestOrchestratorReturnType(t *testing.T) {
	var orch *Orchestrator
	if orch != nil {
		// This line confirms the return type compiles correctly.
		for ev, err := range orch.Run(nil, "", "", "", nil) { //nolint:all
			_ = ev.Name
			var _ string = ev.Name
			// stream.SSEEvent has a Name field of type string.
			switch ev.Data.(type) {
			case stream.AgentTokenData, stream.DagPlanData, stream.NodeStartData:
			}
			if err != nil {
				break
			}
		}
	}
	t.Log("return type is stream.SSEEvent")
}

package orchestrator

import (
	"context"
	"testing"

	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// TestLastOutput verifies that lastOutput picks the terminal node's result.
func TestLastOutput(t *testing.T) {
	plan := &dag.Plan{
		Nodes: []dag.Node{
			{ID: "n1", AgentName: "web-researcher", DependsOn: nil},
			{ID: "n2", AgentName: "synthesizer", DependsOn: []string{"n1"}},
		},
	}
	outputs := map[string]string{"n1": "research result", "n2": "final answer"}
	got := lastOutput(plan, outputs)
	if got != "final answer" {
		t.Errorf("lastOutput = %q, want %q", got, "final answer")
	}
}

func TestLastOutputSingleNode(t *testing.T) {
	plan := &dag.Plan{
		Nodes: []dag.Node{
			{ID: "n1", AgentName: "web-researcher"},
		},
	}
	outputs := map[string]string{"n1": "only answer"}
	got := lastOutput(plan, outputs)
	if got != "only answer" {
		t.Errorf("lastOutput = %q, want %q", got, "only answer")
	}
}

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
	got := o.buildHistory(ctx, userID, sessionID)

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
	if got := o.buildHistory(context.Background(), "u", "missing"); got != nil {
		t.Errorf("buildHistory on missing session = %+v, want nil", got)
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

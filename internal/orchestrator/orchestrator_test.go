package orchestrator

import (
	"context"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
)

// appendUserText / appendModelText add a turn-shaped event to a session.
func appendEvent(t *testing.T, svc session.Service, sess session.Session, author, text string, thought bool) {
	t.Helper()
	ev := session.NewEvent(context.Background(), "test")
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
	appendEvent(t, svc, sess, "orchestrator", "let me think", true) // thinking - dropped
	appendEvent(t, svc, sess, "orchestrator", "Illinois or Missouri?", false)
	appendEvent(t, svc, sess, "user", "Illinois", false)

	o := &Orchestrator{sessions: svc}
	got := buildHistory(o.PriorEvents(ctx, userID, sessionID))

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
	if got := buildHistory(o.PriorEvents(context.Background(), "u", "missing")); got != nil {
		t.Errorf("buildHistory on missing session = %+v, want nil", got)
	}
}

// appendPartsEvent appends a model-authored event carrying arbitrary parts.
func appendPartsEvent(t *testing.T, svc session.Service, sess session.Session, longRunningIDs []string, parts ...*genai.Part) {
	t.Helper()
	ev := session.NewEvent(context.Background(), "test")
	ev.Author = "orchestrator"
	ev.LongRunningToolIDs = longRunningIDs
	ev.Content = &genai.Content{Role: "model", Parts: parts}
	if err := svc.AppendEvent(context.Background(), sess, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// TestPendingChoice verifies a pending get_user_choice call (and its question
// text) is detected while unanswered and clears once a real answer (carrying
// the answer key) follows - so the orchestrator resumes the right turn exactly
// once.
func TestPendingChoice(t *testing.T) {
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
	if id, q := pendingChoice(o.PriorEvents(ctx, userID, sessionID)); id != "" || q != "" {
		t.Errorf("fresh session pendingChoice = (%q, %q), want empty", id, q)
	}

	// The orchestrator asks: a long-running choice call + its auto pending
	// placeholder response (note: NO answer key).
	appendPartsEvent(t, svc, sess, []string{callID},
		&genai.Part{FunctionCall: &genai.FunctionCall{ID: callID, Name: tools.ChoiceToolName, Args: map[string]any{
			"question": "which Springfield?", "options": []string{"Illinois", "Missouri"},
		}}})
	appendPartsEvent(t, svc, sess, nil,
		&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: callID, Name: tools.ChoiceToolName, Response: map[string]any{"status": "pending"}}})

	if id, q := pendingChoice(o.PriorEvents(ctx, userID, sessionID)); id != callID || q != "which Springfield?" {
		t.Errorf("after ask, pendingChoice = (%q, %q), want (%q, %q)", id, q, callID, "which Springfield?")
	}

	// The user answers: a FunctionResponse carrying the answer key resolves it.
	appendPartsEvent(t, svc, sess, nil,
		&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: callID, Name: tools.ChoiceToolName, Response: map[string]any{tools.ChoiceAnswerKey: "Illinois"}}})

	if id, q := pendingChoice(o.PriorEvents(ctx, userID, sessionID)); id != "" || q != "" {
		t.Errorf("after answer, pendingChoice = (%q, %q), want empty", id, q)
	}
}

// TestLatestPendingQuestion verifies the shared helper (used by both Run's
// resume dispatch and the REST status handler) reports a mid-node interrupt
// with its node ID, a top-level clarification with just its message, and
// nothing when neither is pending.
func TestLatestPendingQuestion(t *testing.T) {
	svc := session.InMemoryService()
	ctx := context.Background()
	const userID, sessionID, callID = "u1", "c1", "call-xyz"
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: AppName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess := resp.Session
	o := &Orchestrator{sessions: svc}

	if _, ok := LatestPendingQuestion(o.PriorEvents(ctx, userID, sessionID)); ok {
		t.Fatal("fresh session: expected no pending question")
	}

	// A top-level clarification is pending.
	appendPartsEvent(t, svc, sess, []string{callID},
		&genai.Part{FunctionCall: &genai.FunctionCall{ID: callID, Name: tools.ChoiceToolName, Args: map[string]any{"question": "which Springfield?"}}})
	appendPartsEvent(t, svc, sess, nil,
		&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: callID, Name: tools.ChoiceToolName, Response: map[string]any{"status": "pending"}}})

	pq, ok := LatestPendingQuestion(o.PriorEvents(ctx, userID, sessionID))
	if !ok || pq.Message != "which Springfield?" {
		t.Fatalf("got %+v ok=%v, want message %q", pq, ok, "which Springfield?")
	}
	if _, isNode := pq.NodeInterrupt(); isNode {
		t.Fatal("a top-level clarification should not report as a node interrupt")
	}

	// A mid-node interrupt takes priority over a (stale, already-answered in
	// this scenario it would be irrelevant) top-level clarification.
	ev := &session.Event{}
	ev.RequestedInput = &session.RequestInput{InterruptID: "hitl-n1-r1", Message: "which direction?"}
	nodeEvents := append(append([]*session.Event{}, o.PriorEvents(ctx, userID, sessionID)...), ev)
	pq, ok = LatestPendingQuestion(nodeEvents)
	if !ok || pq.Message != "which direction?" {
		t.Fatalf("got %+v ok=%v, want node message %q", pq, ok, "which direction?")
	}
	if pend, isNode := pq.NodeInterrupt(); !isNode || pend.nodeID != "n1" {
		t.Fatalf("expected node interrupt for n1, got %+v isNode=%v", pend, isNode)
	}
}

// TestOrchestratorReturnType is a compile-time check that Run returns SSEEvent,
// not *session.Event. A later integration test in internal/agent covers the full
// A2A round trip; this package test only covers the orchestrator's own logic.
func TestOrchestratorReturnType(t *testing.T) {
	var orch *Orchestrator
	if orch != nil {
		// This line confirms the return type compiles correctly.
		for ev, err := range orch.Run(nil, "", "", "", "", nil) { //nolint:all
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

// TestLatestPendingNodeInterrupt: an unanswered mid-node HITL request routes the
// next message as its answer; an answered one does not; the most recent
// unanswered request wins; non-hitl RequestedInput events are ignored.
func TestLatestPendingNodeInterrupt(t *testing.T) {
	req := func(id, msg string) *session.Event {
		ev := &session.Event{}
		ev.RequestedInput = &session.RequestInput{InterruptID: id, Message: msg}
		return ev
	}
	ans := func(id string) *session.Event {
		ev := &session.Event{}
		ev.Author = "user"
		ev.Content = &genai.Content{Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{ID: id, Name: workflow.WorkflowInputFunctionCallName},
		}}}
		return ev
	}

	t.Run("pending", func(t *testing.T) {
		p, ok := latestPendingNodeInterrupt([]*session.Event{req("hitl-n1-r1", "which?")})
		if !ok || p.nodeID != "n1" || p.id != "hitl-n1-r1" || p.message != "which?" {
			t.Fatalf("got %+v ok=%v", p, ok)
		}
	})
	t.Run("answered is not pending", func(t *testing.T) {
		if _, ok := latestPendingNodeInterrupt([]*session.Event{req("hitl-n1-r1", "q"), ans("hitl-n1-r1")}); ok {
			t.Fatal("answered interrupt should not be pending")
		}
	})
	t.Run("latest unanswered wins", func(t *testing.T) {
		p, ok := latestPendingNodeInterrupt([]*session.Event{
			req("hitl-n1-r1", "q1"), ans("hitl-n1-r1"), req("hitl-n2-r1", "q2"),
		})
		if !ok || p.nodeID != "n2" {
			t.Fatalf("got %+v ok=%v", p, ok)
		}
	})
	t.Run("non-hitl interrupts ignored", func(t *testing.T) {
		if _, ok := latestPendingNodeInterrupt([]*session.Event{req("something-else", "q")}); ok {
			t.Fatal("non-hitl interrupt should be ignored")
		}
	})
	t.Run("multi-round node id parses", func(t *testing.T) {
		p, ok := latestPendingNodeInterrupt([]*session.Event{req("hitl-research-web-r2", "q")})
		if !ok || p.nodeID != "research-web" {
			t.Fatalf("got %+v ok=%v", p, ok)
		}
	})
}

// TestRunDeadlineExcludesQueueWait pins the failure that killed three M3
// implement runs: the deadline used to start at dispatch, so a run queued
// behind others spent its whole budget WAITING and hit the wall having
// delivered nothing. The clock must start only once a run slot is held.
func TestRunDeadlineExcludesQueueWait(t *testing.T) {
	o := &Orchestrator{}
	o.SetMaxActiveRuns(1)
	o.SetRunDeadline(50 * time.Millisecond)

	// Occupy the only slot, so the next acquire has to wait.
	holderRelease, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("failed to occupy the only slot")
	}

	waited := make(chan bool, 1)
	go func() {
		// A caller context with no deadline of its own - the queue wait must be
		// bounded by the caller, never by the run deadline.
		release, acquired := o.acquireRun(context.Background())
		defer release()
		waited <- acquired
	}()

	// Hold the slot for well past the run deadline. If the deadline covered the
	// wait, the acquire below would never succeed.
	time.Sleep(150 * time.Millisecond)
	holderRelease()

	select {
	case acquired := <-waited:
		if !acquired {
			t.Fatal("queued run failed to acquire a slot after waiting longer than the run deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("queued run never acquired a slot")
	}
}

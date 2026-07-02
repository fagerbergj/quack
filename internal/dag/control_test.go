package dag

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/vetting"
)

// coopStub records worker + judge calls and blocks the FIRST worker call until the
// test unblocks it — so the test can set a cancel/steer via the executor before
// the gate reaches its next stage boundary. The judge always passes.
type coopStub struct {
	mu          sync.Mutex
	workerCalls int
	prompts     []string
	judgeCalls  int
	started     chan struct{}
	unblock     chan struct{}
}

func (*coopStub) Name() string { return "coopStub" }

func (s *coopStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			s.mu.Lock()
			s.judgeCalls++
			s.mu.Unlock()
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		s.workerCalls++
		n := s.workerCalls
		s.prompts = append(s.prompts, gUserText(req))
		s.mu.Unlock()
		if n == 1 {
			select {
			case s.started <- struct{}{}:
			default:
			}
			<-s.unblock // let the test inject cancel/steer before the gate boundary
		}
		yield(gText("draft"), nil)
	}
}

func newCoopExecutor(t *testing.T, stub *coopStub, rounds int) (*Executor, Plan) {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer."})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: rounds} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}
	return ex, plan
}

func drain(t *testing.T, ex *Executor, plan Plan) {
	t.Helper()
	out := map[string]string{}
	for _, err := range ex.Execute(context.Background(), plan, "u", "chat", out) {
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	}
}

// TestExecute_CancelNodeStopsBeforeJudge: cancelling a running node makes the gate
// stop at its next stage boundary — so the judge never runs.
func TestExecute_CancelNodeStopsBeforeJudge(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	go func() {
		<-stub.started
		ex.CancelNode("chat", "n1") // set cancel while the worker is mid-draft
		close(stub.unblock)         // worker finishes → gate checks cancel before judging
	}()
	drain(t, ex, plan)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.judgeCalls != 0 {
		t.Errorf("judge ran %d times; cancel should have stopped the node before judging", stub.judgeCalls)
	}
}

// TestExecute_SteerNodeReRunsWithGuidance: steering a running node re-runs its
// worker with the guidance appended, then proceeds to the judge.
func TestExecute_SteerNodeReRunsWithGuidance(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	go func() {
		<-stub.started
		ex.SteerNode("chat", "n1", "focus on cost") // steer while the worker is mid-draft
		close(stub.unblock)
	}()
	drain(t, ex, plan)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.workerCalls != 2 {
		t.Fatalf("worker ran %d times; steer should trigger a second (guided) run", stub.workerCalls)
	}
	if !strings.Contains(stub.prompts[1], "focus on cost") {
		t.Errorf("re-run prompt missing the steer guidance: %q", stub.prompts[1])
	}
	if stub.judgeCalls != 1 {
		t.Errorf("judge ran %d times; expected 1 after the steered re-run", stub.judgeCalls)
	}
}

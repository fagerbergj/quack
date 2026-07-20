package dag

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// setupStub is a trivial worker: it answers immediately (no tools, no judge
// loop needed since these tests run with a zero vetting.Config).
type setupStub struct {
	mu    sync.Mutex
	calls int
}

func (*setupStub) Name() string { return "setupStub" }
func (s *setupStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		yield(gText("done"), nil)
	}
}

func (s *setupStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// --- (c) plan.Setup == nil: setupQualifyingNodes / runPlanSetup are untouched ---

func TestSetupQualifyingNodes(t *testing.T) {
	plan := Plan{Nodes: []Node{
		{ID: "explore", AgentName: explorerAgent},
		{ID: "impl", AgentName: implementerAgent},
		{ID: "review", AgentName: reviewerAgent},
		{ID: "synth", AgentName: "synthesizer"},
	}}
	got := setupQualifyingNodes(plan)
	if len(got) != 2 || got[0].ID != "impl" || got[1].ID != "review" {
		t.Fatalf("setupQualifyingNodes = %+v, want exactly [impl, review]", got)
	}
}

func TestIsReviewOnlySetup(t *testing.T) {
	tests := []struct {
		name string
		plan Plan
		want bool
	}{
		{"no qualifying nodes", Plan{Nodes: []Node{{ID: "explore", AgentName: explorerAgent}}}, false},
		{"implementer only", Plan{Nodes: []Node{{ID: "impl", AgentName: implementerAgent}}}, false},
		{"reviewer only", Plan{Nodes: []Node{{ID: "review", AgentName: reviewerAgent}}}, true},
		{"implementer then reviewer (implement chain)", Plan{Nodes: []Node{
			{ID: "impl", AgentName: implementerAgent},
			{ID: "review", AgentName: reviewerAgent, DependsOn: []string{"impl"}},
		}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReviewOnlySetup(tt.plan); got != tt.want {
				t.Errorf("isReviewOnlySetup(%+v) = %v, want %v", tt.plan.Nodes, got, tt.want)
			}
		})
	}
}

// runPlanSetup must compute CheckoutExistingHead from the plan's qualifying
// nodes and pass it to setupFn — review-only true, anything with an
// implementer false — even though it is never planner-declared JSON.
func TestRunPlanSetup_ComputesCheckoutExistingHead(t *testing.T) {
	tests := []struct {
		name  string
		nodes []Node
		want  bool
	}{
		{"reviewer only", []Node{{ID: "review", AgentName: reviewerAgent}}, true},
		{"implementer only", []Node{{ID: "impl", AgentName: implementerAgent}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got bool
			ex := &Executor{setupFn: func(_ context.Context, _, _, _ string, s Setup) error {
				got = s.CheckoutExistingHead
				return nil
			}}
			plan := Plan{
				Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
				Nodes: tt.nodes,
			}
			if err := ex.runPlanSetup(context.Background(), "u", "c", plan); err != nil {
				t.Fatalf("runPlanSetup: %v", err)
			}
			if got != tt.want {
				t.Errorf("setupFn's Setup.CheckoutExistingHead = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunPlanSetup_NilSetupIsNoOp(t *testing.T) {
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error {
		t.Fatal("setupFn must never be called when plan.Setup is nil")
		return nil
	}}
	plan := Plan{Nodes: []Node{{ID: "impl", AgentName: implementerAgent}}}
	if err := ex.runPlanSetup(context.Background(), "u", "c", plan); err != nil {
		t.Fatalf("runPlanSetup: %v, want nil (no Setup declared)", err)
	}
}

func TestRunPlanSetup_NoQualifyingNodeIsNoOp(t *testing.T) {
	called := false
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error {
		called = true
		return nil
	}}
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "research", AgentName: "researcher"}},
	}
	if err := ex.runPlanSetup(context.Background(), "u", "c", plan); err != nil {
		t.Fatalf("runPlanSetup: %v", err)
	}
	if called {
		t.Error("setupFn was called though no node in the plan can use a clone")
	}
}

// --- (a) setup provisions ONE shared dir, before anything else ---

func TestRunPlanSetup_ProvisionsOneSharedDir(t *testing.T) {
	var mu sync.Mutex
	var calls []struct{ userID, chatID, dir string }
	ex := &Executor{setupFn: func(_ context.Context, userID, chatID, dir string, s Setup) error {
		mu.Lock()
		defer mu.Unlock()
		if s.Repo != "https://github.com/o/r" || s.BaseRef != "main" || s.WorkBranch != "quack/work" {
			t.Errorf("setupFn got %+v, want the plan's declared Setup", s)
		}
		calls = append(calls, struct{ userID, chatID, dir string }{userID, chatID, dir})
		return nil
	}}
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{
			{ID: "explore", AgentName: explorerAgent},
			{ID: "impl", AgentName: implementerAgent},
		},
	}
	if err := ex.runPlanSetup(context.Background(), "u1", "chat1", plan); err != nil {
		t.Fatalf("runPlanSetup: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("setupFn called %d times, want exactly 1 (one shared clone for the whole plan)", len(calls))
	}
	c := calls[0]
	if c.userID != "u1" || c.chatID != "chat1" {
		t.Errorf("setupFn identity = %+v, want userID=u1 chatID=chat1", c)
	}
	if want := workspace.SetupCloneDir(workspace.SharedRepoScope); c.dir != want {
		t.Errorf("setupFn dir = %q, want %q (the one shared clone dir)", c.dir, want)
	}
}

// A chain of TWO qualifying nodes still gets exactly ONE clone — the whole
// point of the shared-branch design (#310).
func TestRunPlanSetup_ChainOfTwoStillProvisionsOnlyOnce(t *testing.T) {
	var calls int32Counter
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error {
		calls.inc()
		return nil
	}}
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{
			{ID: "impl1", AgentName: implementerAgent},
			{ID: "impl2", AgentName: implementerAgent, DependsOn: []string{"impl1"}},
		},
	}
	if err := ex.runPlanSetup(context.Background(), "u1", "chat1", plan); err != nil {
		t.Fatalf("runPlanSetup: %v", err)
	}
	if got := calls.get(); got != 1 {
		t.Fatalf("setupFn called %d times, want exactly 1 for a 2-node chain", got)
	}
}

func TestRunPlanSetup_IncompleteDeclarationErrors(t *testing.T) {
	called := false
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error {
		called = true
		return nil
	}}
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/o/r", WorkBranch: "quack/work"}, // base_ref missing
		Nodes: []Node{{ID: "impl", AgentName: implementerAgent}},
	}
	if err := ex.runPlanSetup(context.Background(), "u", "c", plan); err == nil {
		t.Fatal("expected an error for an incomplete Setup declaration")
	}
	if called {
		t.Error("setupFn must not run against an incomplete declaration")
	}
}

func TestRunPlanSetup_NoExecutorConfiguredErrors(t *testing.T) {
	ex := &Executor{} // setupFn unset
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "impl", AgentName: implementerAgent}},
	}
	if err := ex.runPlanSetup(context.Background(), "u", "c", plan); err == nil {
		t.Fatal("expected an error: the plan declares setup but no setup executor is wired")
	}
}

// --- (b) a failing setup aborts the run BEFORE any node executes ---

func TestRunPlanAsGraph_FailingSetupAbortsBeforeAnyNodeRuns(t *testing.T) {
	stub := &setupStub{}
	ag, err := llmagent.New(llmagent.Config{Name: implementerAgent, Model: stub, Description: "impl", Instruction: "ROLE Answer."})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{implementerAgent: ag}, map[string]model.LLM{implementerAgent: stub},
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{} }, nil)
	wantErr := errors.New("clone denied")
	ex.SetSetup(func(context.Context, string, string, string, Setup) error { return wantErr })

	plan := Plan{
		ID: "p", UserMessage: "go",
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "impl", AgentName: implementerAgent, Task: "do it"}},
	}
	events, _ := []stream.SSEEvent{}, map[string]string{}
	yield := func(ev stream.SSEEvent, _ error) bool { events = append(events, ev); return true }
	outputs := map[string]string{}
	_, err = ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "chat", nil, yield, outputs, nil)
	if err == nil {
		t.Fatal("expected the failing setup to abort the run with an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("run error = %v, want it to wrap %v", err, wantErr)
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("worker model called %d times, want 0 — the run must abort BEFORE any node executes", got)
	}
	if len(events) != 0 {
		t.Errorf("events = %v, want none — nothing should have started", events)
	}
}

// --- (a) end-to-end: a fresh RunPlanAsGraph call runs setup exactly once, and
// never again on the resume of an already-provisioned plan. ---

func TestRunPlanAsGraph_RunsSetupOnceNotOnResume(t *testing.T) {
	stub := &setupStub{}
	ag, err := llmagent.New(llmagent.Config{Name: implementerAgent, Model: stub, Description: "impl", Instruction: "ROLE Answer."})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{implementerAgent: ag}, map[string]model.LLM{implementerAgent: stub},
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{} }, nil)
	var setupCalls int32Counter
	ex.SetSetup(func(context.Context, string, string, string, Setup) error {
		setupCalls.inc()
		return nil
	})
	plan := Plan{
		ID: "p", UserMessage: "go",
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "impl", AgentName: implementerAgent, Task: "do it"}},
	}
	outputs := map[string]string{}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "chat", nil, func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := setupCalls.get(); got != 1 {
		t.Fatalf("setupFn called %d times on the fresh run, want 1", got)
	}
	if outputs["impl"] != "done" {
		t.Fatalf("outputs = %v, want the implementer's answer", outputs)
	}
}

// int32Counter is a tiny race-free counter (avoids importing sync/atomic just
// for two methods in a test file).
type int32Counter struct {
	mu sync.Mutex
	n  int
}

func (c *int32Counter) inc()     { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *int32Counter) get() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

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
	if len(got) != 3 || got[0].ID != "explore" || got[1].ID != "impl" || got[2].ID != "review" {
		t.Fatalf("setupQualifyingNodes = %+v, want exactly [explore, impl, review]", got)
	}
}

// TestIsReviewOnlySetup pins the full truth table from #555: an explorer can
// no more create a branch than a reviewer can, so it must not flip
// review-only to false, but an implementer anywhere in the set always does.
// An explorer-only plan is NOT review-only: that is the plan-only/research
// shape run against an ISSUE, which has no PR head to check out. Classifying
// it as a review made OverrideReviewWorkBranch demand a head ref that never
// exists there, and the planner thrashed against the error (NightsOut#57).
func TestIsReviewOnlySetup(t *testing.T) {
	tests := []struct {
		name string
		plan Plan
		want bool
	}{
		{"no qualifying nodes", Plan{Nodes: []Node{{ID: "synth", AgentName: "synthesizer"}}}, false},
		{"explorer only (plan-only shape: no PR, no head to check out)", Plan{Nodes: []Node{{ID: "explore", AgentName: explorerAgent}}}, false},
		{"implementer only", Plan{Nodes: []Node{{ID: "impl", AgentName: implementerAgent}}}, false},
		{"reviewer only", Plan{Nodes: []Node{{ID: "review", AgentName: reviewerAgent}}}, true},
		{"explorer + reviewer, no implementer", Plan{Nodes: []Node{
			{ID: "explore", AgentName: explorerAgent},
			{ID: "review", AgentName: reviewerAgent},
		}}, true},
		{"explorer + implementer", Plan{Nodes: []Node{
			{ID: "explore", AgentName: explorerAgent},
			{ID: "impl", AgentName: implementerAgent},
		}}, false},
		{"implementer then reviewer (implement chain)", Plan{Nodes: []Node{
			{ID: "impl", AgentName: implementerAgent},
			{ID: "review", AgentName: reviewerAgent, DependsOn: []string{"impl"}},
		}}, false},
		{"explorer, implementer, reviewer all present", Plan{Nodes: []Node{
			{ID: "explore", AgentName: explorerAgent},
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

// TestOverrideReviewWorkBranch pins #520: a review of a PR with head
// "feat/oidc-auth" must end up with Setup.WorkBranch == "feat/oidc-auth", not
// whatever the planner invented (e.g. "quack-auto-review/review-pr-520",
// which doesn't exist as a remote ref and fatals the setup fetch).
func TestOverrideReviewWorkBranch(t *testing.T) {
	reviewPlan := func(workBranch string) *Plan {
		return &Plan{
			Nodes: []Node{{ID: "review", AgentName: reviewerAgent}},
			Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: workBranch},
		}
	}
	implementPlan := func(workBranch string) *Plan {
		return &Plan{
			Nodes: []Node{{ID: "impl", AgentName: implementerAgent}},
			Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: workBranch},
		}
	}

	t.Run("review plan: forces the real head, overriding the planner's invented name", func(t *testing.T) {
		p := reviewPlan("quack-auto-review/review-pr-520")
		if err := OverrideReviewWorkBranch(p, "feat/oidc-auth"); err != nil {
			t.Fatalf("OverrideReviewWorkBranch: %v", err)
		}
		if p.Setup.WorkBranch != "feat/oidc-auth" {
			t.Errorf("Setup.WorkBranch = %q, want %q", p.Setup.WorkBranch, "feat/oidc-auth")
		}
	})

	t.Run("review plan: errors rather than keeping an invented name when headRef is unknown", func(t *testing.T) {
		p := reviewPlan("quack-auto-review/review-pr-520")
		if err := OverrideReviewWorkBranch(p, ""); err == nil {
			t.Fatal("want an error when no head ref is available, got nil")
		}
		if p.Setup.WorkBranch != "quack-auto-review/review-pr-520" {
			t.Errorf("Setup.WorkBranch changed to %q on error, want it untouched", p.Setup.WorkBranch)
		}
	})

	t.Run("implement plan: untouched, keeps the planner's new-branch name", func(t *testing.T) {
		p := implementPlan("quack/new-feature")
		if err := OverrideReviewWorkBranch(p, "feat/oidc-auth"); err != nil {
			t.Fatalf("OverrideReviewWorkBranch: %v", err)
		}
		if p.Setup.WorkBranch != "quack/new-feature" {
			t.Errorf("Setup.WorkBranch = %q, want unchanged %q", p.Setup.WorkBranch, "quack/new-feature")
		}
	})

	t.Run("no setup: no-op", func(t *testing.T) {
		p := &Plan{Nodes: []Node{{ID: "review", AgentName: reviewerAgent}}}
		if err := OverrideReviewWorkBranch(p, "feat/oidc-auth"); err != nil {
			t.Fatalf("OverrideReviewWorkBranch: %v", err)
		}
	})
}

// runPlanSetup must compute CheckoutExistingHead from the plan's qualifying
// nodes and pass it to setupFn - review-only true, anything with an
// implementer false - even though it is never planner-declared JSON.
func TestRunPlanSetup_ComputesCheckoutExistingHead(t *testing.T) {
	tests := []struct {
		name  string
		nodes []Node
		want  bool
	}{
		{"reviewer only", []Node{{ID: "review", AgentName: reviewerAgent}}, true},
		{"implementer only", []Node{{ID: "impl", AgentName: implementerAgent}}, false},
		{"explorer only", []Node{{ID: "explore", AgentName: explorerAgent}}, false},
		{"explorer + implementer", []Node{{ID: "explore", AgentName: explorerAgent}, {ID: "impl", AgentName: implementerAgent}}, false},
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

// TestRunPlanSetup_ExplorerOnlyProvisionsClone pins #555: a plan whose ONLY
// repo-touching node is an explorer must still provision the shared clone -
// before the fix, setupQualifyingNodes excluded explorers entirely, so
// runPlanSetup no-op'd and the explorer ran in an empty directory.
func TestRunPlanSetup_ExplorerOnlyProvisionsClone(t *testing.T) {
	called := false
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error {
		called = true
		return nil
	}}
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "explore", AgentName: explorerAgent}},
	}
	if err := ex.runPlanSetup(context.Background(), "u", "c", plan); err != nil {
		t.Fatalf("runPlanSetup: %v", err)
	}
	if !called {
		t.Error("setupFn was never called though the plan's only node is an explorer, which qualifies")
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

// A chain of TWO qualifying nodes still gets exactly ONE clone - the whole
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
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{implementerAgent: ag}, map[string]model.LLM{implementerAgent: stub}, nil,
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
		t.Errorf("worker model called %d times, want 0 - the run must abort BEFORE any node executes", got)
	}
	if len(events) != 0 {
		t.Errorf("events = %v, want none - nothing should have started", events)
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
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{implementerAgent: ag}, map[string]model.LLM{implementerAgent: stub}, nil,
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

// TestOverrideReviewWorkBranchIgnoresExplorerOnlyPlan pins the NightsOut#57
// regression: a plan-only run on an ISSUE (explorers grounding a plan, no
// reviewer, no implementer) has no PR and so no head ref. Once explorers
// became setup-qualifying nodes (#556), "no implementer" alone read as
// review-only and this errored out - the planner then thrashed against the
// failure and posted its raw reasoning as the plan.
func TestOverrideReviewWorkBranchIgnoresExplorerOnlyPlan(t *testing.T) {
	p := &Plan{
		Nodes: []Node{{ID: "explore", AgentName: explorerAgent}},
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "plan/investigate"},
	}
	if err := OverrideReviewWorkBranch(p, ""); err != nil {
		t.Fatalf("explorer-only plan with no head ref must not error: %v", err)
	}
	if p.Setup.WorkBranch != "plan/investigate" {
		t.Errorf("work branch = %q, want the planner's own name untouched", p.Setup.WorkBranch)
	}
}

// TestOverrideReviewWorkBranchStillGuardsRealReview keeps #520's protection:
// a genuine PR review (reviewer node) with no head ref must still fail loudly
// rather than fetching an invented branch name.
func TestOverrideReviewWorkBranchStillGuardsRealReview(t *testing.T) {
	p := &Plan{
		Nodes: []Node{{ID: "review", AgentName: reviewerAgent}},
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "invented/name"},
	}
	if err := OverrideReviewWorkBranch(p, ""); err == nil {
		t.Fatal("a review-only plan with no head ref must error, not fetch an invented ref")
	}
}

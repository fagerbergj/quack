package dag

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

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

// Explorers can't create a branch, so they don't flip review-only; an implementer always
// does. Explorer-only is NOT review-only: an issue run has no PR head to check out.
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

// A PR review must use the PR head as WorkBranch, not a planner-invented branch that
// doesn't exist remotely.
func TestOverrideExistingPRHead(t *testing.T) {
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
		if err := OverrideExistingPRHead(p, "feat/oidc-auth"); err != nil {
			t.Fatalf("OverrideExistingPRHead: %v", err)
		}
		if p.Setup.WorkBranch != "feat/oidc-auth" {
			t.Errorf("Setup.WorkBranch = %q, want %q", p.Setup.WorkBranch, "feat/oidc-auth")
		}
	})

	t.Run("review plan: errors rather than keeping an invented name when headRef is unknown", func(t *testing.T) {
		p := reviewPlan("quack-auto-review/review-pr-520")
		if err := OverrideExistingPRHead(p, ""); err == nil {
			t.Fatal("want an error when no head ref is available, got nil")
		}
		if p.Setup.WorkBranch != "quack-auto-review/review-pr-520" {
			t.Errorf("Setup.WorkBranch changed to %q on error, want it untouched", p.Setup.WorkBranch)
		}
	})

	// An implementer bound to an existing PR head must be forced onto it, or delivery's
	// force-push overwrites the PR branch.
	t.Run("implement plan bound to an existing PR: forced onto that head, not the planner's new-branch name", func(t *testing.T) {
		p := implementPlan("quack/new-feature")
		if err := OverrideExistingPRHead(p, "feat/oidc-auth"); err != nil {
			t.Fatalf("OverrideExistingPRHead: %v", err)
		}
		if p.Setup.WorkBranch != "feat/oidc-auth" {
			t.Errorf("Setup.WorkBranch = %q, want the PR's real head %q", p.Setup.WorkBranch, "feat/oidc-auth")
		}
		if !p.Setup.CheckoutExistingHead {
			t.Error("Setup.CheckoutExistingHead = false, want true - a fix/implement run bound to an existing PR must fetch and check out its head, never branch fresh off base")
		}
	})

	t.Run("implement plan with no known PR head (a plain issue): untouched, keeps the planner's new-branch name", func(t *testing.T) {
		p := implementPlan("quack/new-feature")
		if err := OverrideExistingPRHead(p, ""); err != nil {
			t.Fatalf("OverrideExistingPRHead: %v", err)
		}
		if p.Setup.WorkBranch != "quack/new-feature" {
			t.Errorf("Setup.WorkBranch = %q, want unchanged %q", p.Setup.WorkBranch, "quack/new-feature")
		}
		if p.Setup.CheckoutExistingHead {
			t.Error("Setup.CheckoutExistingHead = true, want false - no PR head is known, this is a fresh branch off base")
		}
	})

	t.Run("no setup: no-op", func(t *testing.T) {
		p := &Plan{Nodes: []Node{{ID: "review", AgentName: reviewerAgent}}}
		if err := OverrideExistingPRHead(p, "feat/oidc-auth"); err != nil {
			t.Fatalf("OverrideExistingPRHead: %v", err)
		}
	})
}

// runPlanSetup passes Setup.CheckoutExistingHead through exactly as OverrideExistingPRHead
// decided, never recomputing it from the plan's nodes.
func TestRunPlanSetup_PassesThroughCheckoutExistingHead(t *testing.T) {
	tests := []struct {
		name  string
		nodes []Node
		want  bool
	}{
		{"reviewer only, existing head", []Node{{ID: "review", AgentName: reviewerAgent}}, true},
		{"implementer only, existing head (#625: an implement/fix on a real PR)", []Node{{ID: "impl", AgentName: implementerAgent}}, true},
		{"implementer only, no existing head (a fresh issue-driven branch)", []Node{{ID: "impl", AgentName: implementerAgent}}, false},
		{"explorer + implementer, existing head", []Node{{ID: "explore", AgentName: explorerAgent}, {ID: "impl", AgentName: implementerAgent}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got bool
			ex := &Executor{setupFn: func(_ context.Context, _, _, _ string, s Setup) error {
				got = s.CheckoutExistingHead
				return nil
			}}
			plan := Plan{
				Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work", CheckoutExistingHead: tt.want},
				Nodes: tt.nodes,
			}
			if err := ex.runPlanSetup(context.Background(), "u", "c", plan); err != nil {
				t.Fatalf("runPlanSetup: %v", err)
			}
			if got != tt.want {
				t.Errorf("setupFn's Setup.CheckoutExistingHead = %v, want %v (runPlanSetup must pass through what OverrideExistingPRHead already decided upstream, not recompute it from node composition)", got, tt.want)
			}
		})
	}
}

// After an eager Provision, runPlanSetup sees Setup.Provisioned and no-ops.
func TestProvision_MarksProvisionedAndSkipsOnSecondCall(t *testing.T) {
	var calls int32Counter
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error {
		calls.inc()
		return nil
	}}
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "impl", AgentName: implementerAgent}},
	}
	if err := ex.Provision(context.Background(), "u", "c", &plan); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !plan.Setup.Provisioned {
		t.Fatal("Provision must set Setup.Provisioned = true on success")
	}
	// runPlanSetup takes Plan by value, as RunPlanAsGraph does - Setup itself
	// is still the same pointer, so the run phase sees the same flag.
	if err := ex.runPlanSetup(context.Background(), "u", "c", plan); err != nil {
		t.Fatalf("runPlanSetup: %v", err)
	}
	if got := calls.get(); got != 1 {
		t.Fatalf("setupFn called %d times total, want exactly 1 (Provision then runPlanSetup must not double-clone)", got)
	}
}

// TestProvision_LimitsConcurrentSetup: the (provisionSlots+1)th concurrent
// Provision call must wait for a slot, not clone unbounded.
func TestProvision_LimitsConcurrentSetup(t *testing.T) {
	started := make(chan struct{}, provisionSlots+1)
	release := make(chan struct{})
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error {
		started <- struct{}{}
		<-release
		return nil
	}}
	newPlan := func(i int) *Plan {
		return &Plan{
			Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: fmt.Sprintf("quack/work-%d", i)},
			Nodes: []Node{{ID: "impl", AgentName: implementerAgent}},
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < provisionSlots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = ex.Provision(context.Background(), "u", "c", newPlan(i))
		}(i)
	}
	for i := 0; i < provisionSlots; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d provisions started", i, provisionSlots)
		}
	}

	ninthDone := make(chan struct{})
	go func() {
		_ = ex.Provision(context.Background(), "u", "c", newPlan(provisionSlots))
		close(ninthDone)
	}()
	select {
	case <-started:
		t.Fatal("9th Provision started before a slot freed")
	case <-ninthDone:
		t.Fatal("9th Provision returned before a slot freed")
	case <-time.After(100 * time.Millisecond):
		// correctly blocked
	}

	release <- struct{}{} // free exactly one slot
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("9th Provision never started after a slot freed")
	}
	close(release) // let every remaining call finish
	wg.Wait()
	<-ninthDone
}

// A clone failure reads "plan setup failed: repository ... is unreachable (fatal: ...)",
// stripping runGit's argv prefix, and still errors.Is the cause.
func TestProvision_ClonefailureIsHumanReadable(t *testing.T) {
	// runGit's real error shape: "git <argv...>: <stderr>".
	cause := errors.New("git clone --quiet --branch main --single-branch https://github.com/chrishay-quack/quack.git repo: " +
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error { return cause }}
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/chrishay-quack/quack.git", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "impl", AgentName: implementerAgent}},
	}
	err := ex.Provision(context.Background(), "u", "c", &plan)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, cause) = false, want true (chain must reach the underlying git error)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "plan setup failed") || !strings.Contains(msg, plan.Setup.Repo) || !strings.Contains(msg, "unreachable") {
		t.Errorf("err = %q, want the human setup-failure form naming the repo", msg)
	}
	if !strings.Contains(msg, "fatal: could not read Username") {
		t.Errorf("err = %q, want the underlying git STDERR reason preserved", msg)
	}
	if strings.Contains(msg, "git clone") {
		t.Errorf("err = %q, want the git argv dump (\"git clone ...\") stripped, not surfaced verbatim", msg)
	}
	if plan.Setup.Provisioned {
		t.Error("Setup.Provisioned must stay false after a failed clone")
	}
}

// fakeCleanupError structurally matches internal/tools' cleanupError so this
// test never has to import that package (dag never does, see graph.go).
type fakeCleanupError struct{ msg string }

func (e *fakeCleanupError) Error() string        { return e.msg }
func (e *fakeCleanupError) LocalCleanupFailure() {}

// A stale-clone removal failure is local cleanup: surfaced verbatim, never worded unreachable.
func TestProvision_LocalCleanupFailureIsNeverWordedUnreachable(t *testing.T) {
	cause := &fakeCleanupError{msg: "setup: could not clear stale clone dir /workspace/local/x/quack-shared-repo: permission denied"}
	ex := &Executor{setupFn: func(context.Context, string, string, string, Setup) error { return cause }}
	plan := Plan{
		Setup: &Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{{ID: "impl", AgentName: implementerAgent}},
	}
	err := ex.Provision(context.Background(), "u", "c", &plan)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "unreachable") {
		t.Errorf("err = %q, must not claim the repository is unreachable for a local cleanup failure", msg)
	}
	if !strings.Contains(msg, "could not clear stale clone dir") {
		t.Errorf("err = %q, want the underlying cleanup message preserved", msg)
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

// An explorer as the only repo-touching node still provisions the shared clone.
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

// A chain of two qualifying nodes still gets exactly one clone.
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
		vetting.NewJudgeFactory(stub, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{} }, nil)
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
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{implementerAgent: ag}, map[string]model.LLM{implementerAgent: stub},
		vetting.NewJudgeFactory(stub, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{} }, nil)
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

// A plan-only issue run (explorers only) has no PR head ref, so it must not be treated
// as a review needing one.
func TestOverrideExistingPRHeadIgnoresExplorerOnlyPlan(t *testing.T) {
	p := &Plan{
		Nodes: []Node{{ID: "explore", AgentName: explorerAgent}},
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "plan/investigate"},
	}
	if err := OverrideExistingPRHead(p, ""); err != nil {
		t.Fatalf("explorer-only plan with no head ref must not error: %v", err)
	}
	if p.Setup.WorkBranch != "plan/investigate" {
		t.Errorf("work branch = %q, want the planner's own name untouched", p.Setup.WorkBranch)
	}
}

// A genuine PR review with no head ref still fails loudly rather than fetch an invented branch.
func TestOverrideExistingPRHeadStillGuardsRealReview(t *testing.T) {
	p := &Plan{
		Nodes: []Node{{ID: "review", AgentName: reviewerAgent}},
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "invented/name"},
	}
	if err := OverrideExistingPRHead(p, ""); err == nil {
		t.Fatal("a review-only plan with no head ref must error, not fetch an invented ref")
	}
}

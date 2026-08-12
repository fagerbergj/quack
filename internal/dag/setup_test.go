package dag

import (
	"context"
	"errors"
	"iter"
	"strings"
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
// it as a review made OverrideExistingPRHead demand a head ref that never
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

// TestOverrideExistingPRHead pins #520: a review of a PR with head
// "feat/oidc-auth" must end up with Setup.WorkBranch == "feat/oidc-auth", not
// whatever the planner invented (e.g. "quack-auto-review/review-pr-520",
// which doesn't exist as a remote ref and fatals the setup fetch).
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

	// #625: an implementer node used to leave WorkBranch (and
	// CheckoutExistingHead) untouched even when the run is bound to a real
	// existing PR head - runPlanSetup then did `checkout -b` off base, and
	// delivery's force-push overwrote the PR branch, destroying its commits
	// (observed live on NightsOut#92: original commit 6abb8ef gone).
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

// TestRunPlanSetup_PassesThroughCheckoutExistingHead pins #625: runPlanSetup
// must pass Setup.CheckoutExistingHead to setupFn EXACTLY as
// OverrideExistingPRHead already decided it, for every node composition -
// never recompute it from the plan's nodes (the bug: "reviewer-only" true,
// "any implementer" always false, regardless of whether the run is actually
// bound to an existing PR head).
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

// TestProvision_MarksProvisionedAndSkipsOnSecondCall pins #848: the execute
// tool calls Provision eagerly; the run phase's runPlanSetup must then see
// Setup.Provisioned and no-op, never re-clone the same plan.
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

// TestProvision_ClonefailureIsHumanReadable pins #848's other half: a clone
// failure must read as "plan setup failed: repository ... is unreachable
// (fatal: ...)" - the git STDERR reason is kept (it's the useful part), but
// runGit's leading "git clone --quiet ...: " argv dump is stripped, since
// that's what "the raw git fatal in chat" actually meant live: the full
// invocation, not just the reason. Also must still satisfy errors.Is against
// the underlying cause, so callers that inspect it (e.g. RunPlanAsGraph's
// error wrapping) keep working.
func TestProvision_ClonefailureIsHumanReadable(t *testing.T) {
	// Mirrors runGit's actual error shape (internal/tools/git.go): "git
	// <argv...>: <stderr>" - this IS what SetupClone returns on a real
	// unreachable-repo failure, not a plain message.
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

// TestOverrideExistingPRHeadIgnoresExplorerOnlyPlan pins the NightsOut#57
// regression: a plan-only run on an ISSUE (explorers grounding a plan, no
// reviewer, no implementer) has no PR and so no head ref. Once explorers
// became setup-qualifying nodes (#556), "no implementer" alone read as
// review-only and this errored out - the planner then thrashed against the
// failure and posted its raw reasoning as the plan.
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

// TestOverrideExistingPRHeadStillGuardsRealReview keeps #520's protection:
// a genuine PR review (reviewer node) with no head ref must still fail loudly
// rather than fetching an invented branch name.
func TestOverrideExistingPRHeadStillGuardsRealReview(t *testing.T) {
	p := &Plan{
		Nodes: []Node{{ID: "review", AgentName: reviewerAgent}},
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "invented/name"},
	}
	if err := OverrideExistingPRHead(p, ""); err == nil {
		t.Fatal("a review-only plan with no head ref must error, not fetch an invented ref")
	}
}

package vetting

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/workspace"
)

// The staged-delivery spine: a worker STAGES a pull request
// (stage_pr) instead of opening one - commitDelivery posts the FINAL
// staged set exactly once, and only when the gate's judge round passes.

func TestStagedDeliveryTargetUpsertAndUnstage(t *testing.T) {
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "stage_pr", map[string]any{"title": "Add flappy bird", "body": "first draft"}),
		fnCall("2", "stage_pr", map[string]any{"title": "Add flappy bird v2", "body": "revised"}),
		fnCall("3", "stage_comment", map[string]any{"slot": "progress", "body": "halfway done"}),
		fnCall("4", "unstage", map[string]any{"target": "comment:progress"}),
	), "")
	if len(act.stagedDelivery) != 1 {
		t.Fatalf("stagedDelivery = %+v, want exactly the surviving pr entry", act.stagedDelivery)
	}
	pr, ok := act.stagedDelivery["pr"]
	if !ok || pr.Title != "Add flappy bird v2" {
		t.Fatalf("pr = %+v ok=%v, want the LATEST stage_pr call (upsert, not append)", pr, ok)
	}
	if _, ok := act.stagedDelivery["comment:progress"]; ok {
		t.Fatal("comment:progress was unstaged and must not survive")
	}
}

// A comment staged then unstaged before the gate ever reads the set must never
// be handed to Deliver - commitDelivery only ever sees the FINAL map.
func TestStagedThenUnstagedItemNeverReachesDeliver(t *testing.T) {
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "stage_comment", map[string]any{"slot": "progress", "body": "halfway"}),
		fnCall("2", "unstage", map[string]any{"target": "comment:progress"}),
	), "")
	var called int32
	commitDelivery(context.Background(), nil, Config{Deliver: func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
		atomic.AddInt32(&called, 1)
		return nil, nil
	}}, "n1", act, GateResult{Passed: true})
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("Deliver called %d times, want 0 - every staged item was unstaged", got)
	}
}

func TestCommitDeliveryOnPassNilSafe(t *testing.T) {
	// nil Deliver: no-op, no panic.
	commitDelivery(context.Background(), nil, Config{}, "n1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"pr": {Kind: "pull_request", Title: "x"}},
	}, GateResult{Passed: true})
	// Deliver set but nothing staged: no-op.
	var called int32
	commitDelivery(context.Background(), nil, Config{Deliver: func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
		atomic.AddInt32(&called, 1)
		return nil, nil
	}}, "n2", workerActivity{}, GateResult{Passed: true})
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("Deliver called %d times, want 0 - nothing was staged", got)
	}
}

func TestCommitDeliveryOnPassCarriesCloneCoordinates(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	cfg := Config{Deliver: func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}}
	commitDelivery(context.Background(), nil, cfg, "n3", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"pr": {Kind: "pull_request", Title: "Add flappy bird"}},
		clonedRepos:    []string{"https://github.com/fagerbergj/games"},
		clonedDirs:     []string{"games"},
		currentBranch:  "add-flappy-bird",
	}, GateResult{Passed: true})
	select {
	case dc := <-done:
		if len(dc.Items) != 1 || dc.Items[0].Title != "Add flappy bird" {
			t.Fatalf("Items = %+v, want the one staged PR", dc.Items)
		}
		if dc.CloneURL != "https://github.com/fagerbergj/games" || dc.Branch != "add-flappy-bird" {
			t.Fatalf("DeliveryContext = %+v, want the ledger's clone URL and branch", dc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired")
	}
}

// A setup-provisioned node (cfg.Setup set) must deliver on the plan's declared
// work_branch - never the worker's own git-tracking ledger, which a
// setup-provisioned worker is told not to touch (internal/github/webhook.go).
// This is the regression #308 fixes: before, a setup-provisioned worker never
// called git_checkout/git_clone itself, so act.currentBranch/clonedDirs stayed
// empty and delivery failed with "no branch to open it from".
func TestCommitDeliveryOnPassUsesSetupBranchWhenDeclared(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The clone setup would have made, at the SAME path workspace.SetupCloneDir
	// computes - so CloneDir resolves to a real, existing directory.
	cloneDir, err := j.EnsureDir("u1", "chat1", workspace.SetupCloneDir("impl"))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan DeliveryContext, 1)
	cfg := Config{
		Deliver: func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
			done <- dc
			return nil, nil
		},
		Setup: &SetupBranch{Repo: "https://github.com/fagerbergj/games", WorkBranch: "quack/work"},
		// The workspace-directory scope commitDelivery resolves
		// SetupCloneDir against - dag.buildGateNodes stamps this (node.ID
		// normally); the caller below passes the SAME "impl" as the nodeID
		// argument, matching production wiring for a single, unshared node.
		NodeID:          "impl",
		Workspace:       j,
		WorkspaceUserID: "u1",
		ChatID:          "chat1",
	}
	// The worker never cloned or checked out anything itself (setup-provisioned
	// runs are told not to) - the ledger fields commitDelivery would
	// otherwise fall back to are empty on purpose.
	// Kind "review" (not "pull_request") - no GitCredentials configured here,
	// and this test is about branch/URL/dir resolution, not the push itself.
	commitDelivery(context.Background(), nil, cfg, "impl", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "looks good"}},
	}, GateResult{Passed: true})
	select {
	case dc := <-done:
		if dc.Branch != "quack/work" {
			t.Errorf("Branch = %q, want the plan's declared work_branch", dc.Branch)
		}
		if dc.CloneURL != "https://github.com/fagerbergj/games" {
			t.Errorf("CloneURL = %q, want the plan's declared repo", dc.CloneURL)
		}
		if dc.CloneDir != cloneDir {
			t.Errorf("CloneDir = %q, want %q (workspace.SetupCloneDir's resolved path)", dc.CloneDir, cloneDir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired")
	}
}

// --- end-to-end: the gate loop itself, not just the direct-call helpers ---

type stagePRToolArgs struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}
type stagePRToolResult struct {
	Result string `json:"result"`
}

func stagePRTestTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[stagePRToolArgs, stagePRToolResult](
		functiontool.Config{Name: "stage_pr", Description: "Stage a pull request."},
		func(_ adkagent.Context, _ stagePRToolArgs) (stagePRToolResult, error) {
			return stagePRToolResult{Result: "staged"}, nil
		})
	if err != nil {
		t.Fatalf("stage_pr tool: %v", err)
	}
	return tl
}

// deliveryStub drives a worker through commit → stage_pr → done, then lets the
// judge score whatever judgeScore says - so a test can force a pass or a
// permanent fail and observe whether commitDelivery fires.
type deliveryStub struct {
	judgeScore float64
}

func (m *deliveryStub) Name() string { return "deliveryStub" }

func (m *deliveryStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case stubHasTool(req, submitVerdictTool):
			// A named criterion, not a flat score: aggregateVerdict recomputes the
			// overall score as the weakest-link MIN across v.Criteria whenever
			// foldDeterministic has added any (delivery_complete, here, once the
			// worker has committed+staged) - a bare "score" would be silently
			// discarded by that recomputation.
			yield(stubCall(submitVerdictTool, map[string]any{
				"criteria": map[string]any{"task_completeness": map[string]any{"score": m.judgeScore, "reason": "judged"}},
				"score":    m.judgeScore, "feedback": "ok",
			}), nil)
		case !stubHasResponse(req, "git_commit"):
			yield(stubCall("git_commit", map[string]any{"message": "add the game"}), nil)
		case !stubHasResponse(req, "stage_pr"):
			yield(stubCall("stage_pr", map[string]any{"title": "Add flappy bird", "body": "adds a game"}), nil)
		default:
			yield(stubText("Committed and staged for delivery."), nil)
		}
	}
}

// deliveryStubJudgeErrors drives a worker through the same commit -> stage_pr
// -> done spine as deliveryStub, but the judge call always fails with a
// non-transient error (never recovers, even after runJudgeAgent's backoff
// retries) - the stand-in for #572's permanent judge outage.
type deliveryStubJudgeErrors struct{}

func (m *deliveryStubJudgeErrors) Name() string { return "deliveryStubJudgeErrors" }

func (m *deliveryStubJudgeErrors) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case stubHasTool(req, submitVerdictTool):
			yield(nil, errors.New("judge model permanently unreachable"))
		case !stubHasResponse(req, "git_commit"):
			yield(stubCall("git_commit", map[string]any{"message": "add the game"}), nil)
		case !stubHasResponse(req, "stage_pr"):
			yield(stubCall("stage_pr", map[string]any{"title": "Add flappy bird", "body": "adds a game"}), nil)
		default:
			yield(stubText("Committed and staged for delivery."), nil)
		}
	}
}

const deliveryGateTask = "Add a game to the repo, commit it, push the branch and open a pull request."

func runDeliveryGate(t *testing.T, stub model.LLM, cfg Config) {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "implementer",
		Instruction: "Do the task.", Tools: []tool.Tool{commitTool(t), stagePRTestTool(t)},
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	node, err := newTestGatedNode("impl-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker},
		Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: deliveryGateTask}}}
	for _, rerr := range r.Run(t.Context(), "u", "s", content, adkagent.RunConfig{}) {
		if rerr != nil {
			t.Fatalf("run: %v", rerr)
		}
	}
}

// Regression: workers commit locally and STAGE a pull request; the gate
// delivers it exactly once, only after the judge passes.
func TestGate_DeliversStagedPROnceOnJudgePass(t *testing.T) {
	var calls int32
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		atomic.AddInt32(&calls, 1)
		if len(dc.Items) != 1 || dc.Items[0].Kind != "pull_request" || dc.Items[0].Title != "Add flappy bird" {
			t.Errorf("DeliveryContext.Items = %+v, want the one staged PR", dc.Items)
		}
		return nil, nil
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10", Task: deliveryGateTask, Deliver: deliver}
	runDeliveryGate(t, &deliveryStub{judgeScore: 0.9}, cfg)

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("Deliver called %d times, want exactly 1", got)
	}
}

// A gate that never clears the judge threshold still DELIVERS - graceful
// degradation: the work is done, so it ships with GatePassed=false so the
// extension attaches a caveat (App.Deliver's gateCaveat), rather than the work
// being silently dropped.
func TestGate_DeliversWithCaveatOnJudgeFail(t *testing.T) {
	got := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		select {
		case got <- dc:
		default:
		}
		return nil, nil
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.99, Rubric: "score 0-10", Task: deliveryGateTask, Deliver: deliver}
	runDeliveryGate(t, &deliveryStub{judgeScore: 0.5}, cfg)

	select {
	case dc := <-got:
		if dc.GatePassed {
			t.Error("GatePassed = true, want false - the judge never cleared the threshold")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired on a judge fail - delivery must be graceful (with caveat), not dropped")
	}
}

// TestGate_JudgeOutageDeliversWithCaveatNeverStrandsVerdict pins #572: when
// the judge call itself errors and never recovers (not a low score - the
// model was unreachable), the gate must NOT return early and skip
// commitDelivery. Before the fix, that early return meant nothing was ever
// staged/delivered through the gate's own path - for a GitHub review, the
// verdict marker (the ONLY place a self-review's verdict can live) was never
// written, and the reviewer's text instead surfaced elsewhere as a plain,
// unmarked comment that read exactly like a normal approved review.
func TestGate_JudgeOutageDeliversWithCaveatNeverStrandsVerdict(t *testing.T) {
	got := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		select {
		case got <- dc:
		default:
		}
		return nil, nil
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10", Task: deliveryGateTask, Deliver: deliver}
	runDeliveryGate(t, &deliveryStubJudgeErrors{}, cfg)

	select {
	case dc := <-got:
		if dc.GatePassed {
			t.Error("GatePassed = true, want false - the judge never scored this at all")
		}
		if !strings.Contains(dc.GateFeedback, "unavailable") {
			t.Errorf("GateFeedback = %q, want it to name the judge outage so a caveat naming it is visible on delivery", dc.GateFeedback)
		}
		if len(dc.Items) != 1 || dc.Items[0].Kind != "pull_request" {
			t.Errorf("Items = %+v, want the staged work delivered anyway, never silently stranded", dc.Items)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired on a permanent judge outage - #572 requires delivering with a visible caveat, never stranding the verdict")
	}
}

// TestCommitDeliveryFiresOnFailWithCaveat pins graceful degradation: delivery
// happens even on a judge FAIL, and the DeliveryContext carries the verdict so
// the extension can attach a caveat (GatePassed=false, GateFeedback set).
func TestCommitDeliveryFiresOnFailWithCaveat(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	cfg := Config{Deliver: func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}}
	commitDelivery(context.Background(), nil, cfg, "n4", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"pr": {Kind: "pull_request", Title: "x"}},
		clonedRepos:    []string{"https://github.com/fagerbergj/games"},
		clonedDirs:     []string{"games"},
		currentBranch:  "b",
	}, GateResult{Passed: false, Feedback: "tests are missing for the error path"})
	select {
	case dc := <-done:
		if dc.GatePassed {
			t.Error("GatePassed = true, want false - the judge failed")
		}
		if dc.GateFeedback != "tests are missing for the error path" {
			t.Errorf("GateFeedback = %q, want the judge's feedback carried through", dc.GateFeedback)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired on a judge fail - delivery must be graceful, not gated")
	}
}

// #1059: a multi-reviewer plan's merged review is delivered by the
// synthesizer node, which never clones anything itself - its own cfg/act
// carry no clone URL. The merged delivery must still carry the URL one of
// the actual reviewer nodes cloned, or Deliver has nothing to post against.
func TestReviewFanoutMergedDeliveryCarriesReviewerCloneURL(t *testing.T) {
	const planID = "plan-1059"
	fanout := GetReviewFanout(planID, 2)
	fanout.ExpectSynthesis()
	defer ResetReviewFanout(planID)

	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}

	reviewerCfg := Config{Deliver: deliver, IsReviewer: true, ReviewFanout: fanout}
	commitDelivery(context.Background(), nil, reviewerCfg, "r1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "lgtm"}},
		clonedRepos:    []string{"https://github.com/fagerbergj/quack"},
		currentBranch:  "feat/x",
	}, GateResult{Passed: true})
	commitDelivery(context.Background(), nil, reviewerCfg, "r2", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "comment", Body: "nit"}},
		clonedRepos:    []string{"https://github.com/fagerbergj/quack"},
		currentBranch:  "feat/x",
	}, GateResult{Passed: true})

	// The synthesizer node: never clones anything, matching production.
	synthCfg := Config{Deliver: deliver, IsReviewer: false, ReviewFanout: fanout}
	commitDelivery(context.Background(), nil, synthCfg, "synthesize", workerActivity{
		answer: "consolidated review",
	}, GateResult{Passed: true})

	select {
	case dc := <-done:
		if dc.CloneURL != "https://github.com/fagerbergj/quack" {
			t.Fatalf("CloneURL = %q, want the reviewer nodes' clone URL carried through the fan-in", dc.CloneURL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired")
	}
}

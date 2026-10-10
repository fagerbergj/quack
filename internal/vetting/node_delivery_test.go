package vetting

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/workspace"
)

// A worker stages a PR (stage_pr) instead of opening one; commitDelivery posts the final staged
// set exactly once, when the judge round passes.

func TestStagedDeliveryTargetUpsertAndUnstage(t *testing.T) {
	act := activityFromSessionAt(newTestSession(t,
		fnCall("1", "stage_pr", map[string]any{"title": "Add flappy bird", "body": "first draft"}),
		fnCall("2", "stage_pr", map[string]any{"title": "Add flappy bird v2", "body": "revised"}),
		fnCall("3", "stage_comment", map[string]any{"slot": "progress", "body": "halfway done"}),
		fnCall("4", "unstage", map[string]any{"target": "comment:progress"}),
	), "", "")
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
	), "", "")
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

// A setup-provisioned node delivers on the plan's work_branch, not its git ledger, which stays
// empty because such a worker never clones or checks out itself.
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
		// The scope SetupCloneDir resolves against; matches the nodeID passed below, as in production.
		NodeID:          "impl",
		Workspace:       j,
		WorkspaceUserID: "u1",
		ChatID:          "chat1",
	}
	// Ledger fields are empty on purpose. Kind "review" avoids the push (no GitCredentials); this
	// tests branch/URL/dir resolution only.
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

// deliveryStub drives a worker through commit -> stage_pr -> done; judge answers every verdict call.
func deliveryStub(judge func() (*model.LLMResponse, error)) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		switch {
		case stubHasTool(req, submitVerdictTool):
			return judge()
		case !stubHasResponse(req, "git_commit"):
			return stubCall("git_commit", map[string]any{"message": "add the game"}), nil
		case !stubHasResponse(req, "stage_pr"):
			return stubCall("stage_pr", map[string]any{"title": "Add flappy bird", "body": "adds a game"}), nil
		default:
			return stubText("Committed and staged for delivery."), nil
		}
	}
}

// scoredDeliveryStub's judge scores a named criterion: once delivery_complete is folded in,
// aggregateVerdict recomputes the score as the MIN over criteria and drops a bare "score".
func scoredDeliveryStub(score float64) fnLLM {
	return deliveryStub(func() (*model.LLMResponse, error) {
		return stubCall(submitVerdictTool, map[string]any{
			"criteria": map[string]any{"task_completeness": map[string]any{"score": score, "reason": "judged"}},
			"score":    score, "feedback": "ok",
		}), nil
	})
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
	runDeliveryGate(t, scoredDeliveryStub(0.9), cfg)

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("Deliver called %d times, want exactly 1", got)
	}
}

// A gate that never clears the threshold still delivers, with GatePassed=false so the extension
// attaches a caveat instead of the work being dropped.
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
	runDeliveryGate(t, scoredDeliveryStub(0.5), cfg)

	select {
	case dc := <-got:
		if dc.GatePassed {
			t.Error("GatePassed = true, want false - the judge never cleared the threshold")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired on a judge fail - delivery must be graceful (with caveat), not dropped")
	}
}

// A permanent judge outage must not skip commitDelivery, or a review's verdict marker is never
// written and its text reads like an approved review.
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
	runDeliveryGate(t, deliveryStub(func() (*model.LLMResponse, error) {
		return nil, errors.New("judge model permanently unreachable")
	}), cfg)

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

// Delivery happens on a judge FAIL too, carrying GatePassed=false and GateFeedback for a caveat.
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

// A gate push failure still reaches Deliver with dc.PushError: only the extension can tell the
// human on GitHub that delivery failed.
func TestCommitDeliveryStillReachesDeliverOnPushFailure(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.EnsureDir("u1", "chat1", workspace.SetupCloneDir("impl")); err != nil {
		t.Fatal(err)
	}

	done := make(chan DeliveryContext, 1)
	cfg := Config{
		Deliver: func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
			done <- dc
			// A real extension skips attempting push-dependent items once it
			// sees PushError, and reports the failure back to GitHub instead.
			return []DeliveryItemOutcome{{Kind: dc.Items[0].Kind, Error: dc.PushError}}, errors.New(dc.PushError)
		},
		Setup:           &SetupBranch{Repo: "https://github.com/fagerbergj/games", WorkBranch: "quack/work"},
		NodeID:          "impl",
		Workspace:       j,
		WorkspaceUserID: "u1",
		ChatID:          "chat1",
		// GitCredentials left nil on purpose: ensurePush fails with
		// "no git credential source configured" before Deliver is ever called.
	}
	commitDelivery(context.Background(), nil, cfg, "impl", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"pr": {Kind: "pull_request", Title: "x"}},
	}, GateResult{Passed: true})

	select {
	case dc := <-done:
		if dc.PushError == "" {
			t.Fatal("PushError = \"\", want ensurePush's failure carried through so Deliver can report it")
		}
		if len(dc.Items) != 1 {
			t.Fatalf("Items = %+v, want the staged item still present (Deliver decides whether to attempt it)", dc.Items)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired on a push failure - the extension must still get a chance to report it (#1155)")
	}
}

// The synthesizer delivering a merged review never clones, so the delivery must carry a
// reviewer node's clone URL or Deliver has nothing to post against.
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

// proves commitDelivery reads a real code_review record's fields end to
// end, not just mergeReviews' own unit tests against a struct literal.
func TestReviewFanout_SynthesizerCodeReviewRecordFlowsThroughCommitDelivery(t *testing.T) {
	const planID = "plan-record-flow"
	fanout := GetReviewFanout(planID, 1)
	fanout.ExpectSynthesis()
	defer ResetReviewFanout(planID)

	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}
	artifacts := artifact.InMemoryService()
	chatID := "ext:github:owner-repo-1377"

	reviewerCfg := Config{Deliver: deliver, IsReviewer: true, ReviewFanout: fanout, ChatID: chatID, User: "u1", Artifacts: artifacts}
	commitDelivery(context.Background(), nil, reviewerCfg, "code-reviewer-1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review",
			Comments: []ReviewComment{{Path: "internal/dag/executor.go", Line: 149, Body: "blocking: NeedsInput has no caller anywhere."}}}},
	}, GateResult{Passed: true})

	// What write_code_review actually saves - the synthesizer's structured
	// record, not a VERDICT/TAKEAWAY answer tail.
	synthCfg := Config{Deliver: deliver, ReviewFanout: fanout, ChatID: chatID, User: "u1", Artifacts: artifacts}
	rec := CodeReviewRecord{
		Verdict:  "request_changes",
		Takeaway: "One blocking issue across the slices.",
		Verified: []string{"ran go test ./internal/dag/..."},
		Notes:    []string{"worth a follow-up on the doc comment"},
	}
	if _, _, err := recordClient(synthCfg).SaveStructured(context.Background(), kindCodeReview, rec, SubjectHint(chatID), recordstore.Lineage{}); err != nil {
		t.Fatalf("seed code_review: %v", err)
	}
	commitDelivery(context.Background(), nil, synthCfg, "synthesize", workerActivity{
		answer: "Staged: request_changes, 1 blocking.",
	}, GateResult{Passed: true})

	select {
	case dc := <-done:
		body := dc.Items[0].Body
		if !strings.Contains(body, "One blocking issue across the slices.") {
			t.Fatalf("Body missing the record's takeaway: %q", body)
		}
		if !strings.Contains(body, "### Verified") || !strings.Contains(body, "ran go test") {
			t.Fatalf("Body missing the record's Verified section: %q", body)
		}
		if !strings.Contains(body, "### Notes") || !strings.Contains(body, "follow-up") {
			t.Fatalf("Body missing the record's Notes section: %q", body)
		}
		if strings.Contains(body, "Staged: request_changes") {
			t.Fatalf("Body must not carry the synthesizer's own chat reply as narration: %q", body)
		}
		if len(dc.Items[0].Comments) != 1 || strings.HasPrefix(dc.Items[0].Comments[0].Body, "[") {
			t.Fatalf("Comments = %+v, want the finding posted with no slice-id prefix", dc.Items[0].Comments)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver never fired")
	}
}

// A cancel landing as Deliver returns must not lose the delivery_record completion: the GitHub
// side effect already happened.
func TestCommitDelivery_BookkeepingSurvivesCancelAfterDeliver(t *testing.T) {
	cfg := Config{IsReviewer: true, ChatID: "ext:github:owner-repo-1187", User: "u1", Artifacts: artifact.InMemoryService()}
	finding := FindingRecord{Path: "a.go", Title: "x", State: "new"}
	fid, _ := recordstore.IdentityFor(kindFinding, finding, "")
	seedCodeReview(t, cfg, "approve", "s", map[string]FindingRecord{fid: finding})

	// Records whether AppendIntent's ctx was already cancelled; fakeGateLedger ignores ctx.
	shim := &ctxCapturingLedger{fakeGateLedger: newFakeGateLedger()}
	cfg.Ledger = shim

	ctx, cancel := context.WithCancel(context.Background())
	cfg.Deliver = func(context.Context, DeliveryContext) ([]DeliveryItemOutcome, error) {
		cancel() // simulate a shutdown drain firing right as GitHub confirms the post
		return []DeliveryItemOutcome{{Kind: "review", URL: "https://example/review/1"}}, nil
	}
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "x"}}}
	commitDelivery(ctx, nil, cfg, "n1", act, GateResult{Passed: true})

	targetID, _ := recordstore.IdentityFor(kindCodeReview, nil, SubjectHint(cfg.ChatID))
	if entries := listDeliveryRecords(context.Background(), cfg, targetID); len(entries) == 0 {
		t.Fatal("delivery_record was not saved after the run context was cancelled")
	}
	if !shim.sawDeliveryRecord {
		t.Fatal("delivery_record's artifact.revision WAL entry was not appended after the run context was cancelled")
	}
	if shim.doneCtxErr != nil {
		t.Fatalf("delivery_record was appended on a context that was already Done (err=%v) - bookkeeping must run detached from the run cancel", shim.doneCtxErr)
	}
}

// ctxCapturingLedger records ctx.Err() at the delivery_record's AppendIntent, which
// fakeGateLedger ignores.
type ctxCapturingLedger struct {
	*fakeGateLedger
	sawDeliveryRecord bool
	doneCtxErr        error
}

func (c *ctxCapturingLedger) AppendIntent(ctx context.Context, e ledger.Entry) (int64, error) {
	var p struct {
		Kind string `json:"kind"`
	}
	if e.Kind == ledger.KindArtifactRevision && json.Unmarshal(e.Payload, &p) == nil && p.Kind == kindDeliveryRecord {
		c.sawDeliveryRecord = true
		c.doneCtxErr = ctx.Err()
	}
	return c.fakeGateLedger.AppendIntent(ctx, e)
}

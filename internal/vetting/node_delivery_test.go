package vetting

import (
	"context"
	"iter"
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
)

// The staged-delivery spine (0.5.0): a worker STAGES a pull request
// (stage_pr) instead of opening one — commitDeliveryOnPass posts the FINAL
// staged set exactly once, and only when the gate's judge round passes.

func TestStagedDeliveryTargetUpsertAndUnstage(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "stage_pr", map[string]any{"title": "Add flappy bird", "body": "first draft"}),
		fnCall("2", "stage_pr", map[string]any{"title": "Add flappy bird v2", "body": "revised"}),
		fnCall("3", "stage_comment", map[string]any{"slot": "progress", "body": "halfway done"}),
		fnCall("4", "unstage", map[string]any{"target": "comment:progress"}),
	))
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
// be handed to Deliver — commitDeliveryOnPass only ever sees the FINAL map.
func TestStagedThenUnstagedItemNeverReachesDeliver(t *testing.T) {
	act := activityFromSession(newTestSession(t,
		fnCall("1", "stage_comment", map[string]any{"slot": "progress", "body": "halfway"}),
		fnCall("2", "unstage", map[string]any{"target": "comment:progress"}),
	))
	var called int32
	commitDeliveryOnPass(Config{Deliver: func(context.Context, DeliveryContext) error {
		atomic.AddInt32(&called, 1)
		return nil
	}}, "n1", act)
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("Deliver called %d times, want 0 — every staged item was unstaged", got)
	}
}

func TestCommitDeliveryOnPassNilSafe(t *testing.T) {
	// nil Deliver: no-op, no panic.
	commitDeliveryOnPass(Config{}, "n1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"pr": {Kind: "pull_request", Title: "x"}},
	})
	// Deliver set but nothing staged: no-op.
	var called int32
	commitDeliveryOnPass(Config{Deliver: func(context.Context, DeliveryContext) error {
		atomic.AddInt32(&called, 1)
		return nil
	}}, "n2", workerActivity{})
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("Deliver called %d times, want 0 — nothing was staged", got)
	}
}

func TestCommitDeliveryOnPassCarriesCloneCoordinates(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	cfg := Config{Deliver: func(_ context.Context, dc DeliveryContext) error {
		done <- dc
		return nil
	}}
	commitDeliveryOnPass(cfg, "n3", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"pr": {Kind: "pull_request", Title: "Add flappy bird"}},
		clonedRepos:    []string{"https://github.com/fagerbergj/games"},
		clonedDirs:     []string{"games"},
		currentBranch:  "add-flappy-bird",
	})
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
// judge score whatever judgeScore says — so a test can force a pass or a
// permanent fail and observe whether commitDeliveryOnPass fires.
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
			// worker has committed+staged) — a bare "score" would be silently
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
	deliver := func(_ context.Context, dc DeliveryContext) error {
		atomic.AddInt32(&calls, 1)
		if len(dc.Items) != 1 || dc.Items[0].Kind != "pull_request" || dc.Items[0].Title != "Add flappy bird" {
			t.Errorf("DeliveryContext.Items = %+v, want the one staged PR", dc.Items)
		}
		return nil
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

// A gate that never clears the judge threshold must deliver NOTHING — the
// staged PR was staged, never posted.
func TestGate_NeverDeliversOnJudgeFail(t *testing.T) {
	var calls int32
	deliver := func(context.Context, DeliveryContext) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.99, Rubric: "score 0-10", Task: deliveryGateTask, Deliver: deliver}
	runDeliveryGate(t, &deliveryStub{judgeScore: 0.5}, cfg)

	time.Sleep(200 * time.Millisecond) // let any errant goroutine fire
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("Deliver called %d times, want 0 — the judge never passed", got)
	}
}

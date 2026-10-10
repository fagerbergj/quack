package vetting

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// stubPlanJudgeModel always calls submit_plan_verdict with accept/reason.
func stubPlanJudgeModel(accept bool, reason string) fnLLM {
	return func(*model.LLMRequest) (*model.LLMResponse, error) {
		return stubCall(submitPlanVerdictTool, map[string]any{"accept": accept, "reason": reason}), nil
	}
}

// noVerdictModel ends its run without ever calling submit_plan_verdict.
var noVerdictModel = fnLLM(func(*model.LLMRequest) (*model.LLMResponse, error) {
	return &model.LLMResponse{TurnComplete: true}, nil
})

// maxTokensRecordingPlanJudge records the request's MaxOutputTokens into got, then accepts.
func maxTokensRecordingPlanJudge(got *int32) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		if req.Config != nil {
			atomic.StoreInt32(got, req.Config.MaxOutputTokens)
		}
		return stubCall(submitPlanVerdictTool, map[string]any{"accept": true, "reason": ""}), nil
	}
}

// TestPlanJudge_RequestCarriesConfiguredMaxOutputTokens proves the cap
// argument reaches the plan judge's own model request.
func TestPlanJudge_RequestCarriesConfiguredMaxOutputTokens(t *testing.T) {
	got := int32(-1)
	judge := NewPlanJudge(maxTokensRecordingPlanJudge(&got), 1024, "", nil, nil)
	if _, _, err := judge(context.Background(), "write a plan", "1 node(s):\n- explore (web-researcher)", ""); err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if got != 1024 {
		t.Errorf("req.Config.MaxOutputTokens = %d, want 1024", got)
	}
}

// loopingPlanJudgeModel streams one phrase as many partial deltas in ONE call and never submits:
// the plan judge has no other tool, so the runaway must happen inside a single generation.
type loopingPlanJudgeModel struct{ chunks int32 }

func (m *loopingPlanJudgeModel) Name() string { return "looping-plan-judge" }

func (m *loopingPlanJudgeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for i := 0; i < 500; i++ {
			atomic.AddInt32(&m.chunks, 1)
			resp := &model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
					{Text: "This exact sentence repeats without variation. "},
				}},
				Partial: true,
			}
			if !yield(resp, nil) {
				return
			}
		}
	}
}

// The repeat guard aborts a runaway stream well before the fake's 500 chunks, and the round ends
// like any other no-verdict reply.
func TestPlanJudge_RunawayRepeatRoutesToNoVerdict(t *testing.T) {
	m := &loopingPlanJudgeModel{}
	judge := NewPlanJudge(m, 0, "", nil, nil)
	_, _, err := judge(context.Background(), "write a plan", "1 node(s):\n- explore (web-researcher)", "")
	if err == nil {
		t.Fatal("PlanJudge: expected an error - the model never calls submit_plan_verdict")
	}
	if got := atomic.LoadInt32(&m.chunks); got >= 500 {
		t.Errorf("model streamed %d chunks, want fewer than the scripted 500 - the repeat guard should have aborted early", got)
	}
}

// planPromptCapturingModel appends every request text part to capture, then accepts.
func planPromptCapturingModel(capture *string) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p != nil && p.Text != "" {
					*capture += p.Text
				}
			}
		}
		return stubCall(submitPlanVerdictTool, map[string]any{"accept": true, "reason": ""}), nil
	}
}

// Top-k memories for the chat's scope reach the plan-judge prompt and are logged as memory.recall
// with source "plan_judge", but are never voted on.
func TestNewPlanJudge_ProjectMemorySection_LoggedNotVoted(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStoreForVoteTest(t)
	if _, err := store.Commit(ctx, memory.Scope{User: "u1"}, "seed", memory.Provenance{},
		[]memory.Candidate{{Content: "prefers terse commit messages"}}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	lgr := ledgertest.NewMemStore()
	var prompt string
	judge := NewPlanJudge(planPromptCapturingModel(&prompt), 0, "", store, lgr)

	runCtx := ledger.WithCoords(ctx, ledger.Coords{ChatID: "chat-plan", User: "u1"})
	accept, _, err := judge(runCtx, "write a plan", "1 node(s):\n- explore (web-researcher)", "")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if !accept {
		t.Fatal("want accept")
	}
	if !strings.Contains(prompt, "PROJECT MEMORY") || !strings.Contains(prompt, "terse commit messages") {
		t.Fatalf("prompt missing the project-memory section: %q", prompt)
	}

	entries, err := lgr.ReadEntries(ctx, "chat-plan", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	var recalls, votes int
	for _, e := range entries {
		switch e.Kind {
		case ledger.KindMemoryRecall:
			recalls++
			var p ledger.MemoryRecallPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("unmarshal memory.recall payload: %v", err)
			}
			if p.Source != "plan_judge" {
				t.Errorf("memory.recall source = %q, want %q", p.Source, "plan_judge")
			}
		case ledger.KindMemoryVote:
			votes++
		}
	}
	if recalls != 1 {
		t.Fatalf("memory.recall entries = %d, want 1", recalls)
	}
	if votes != 0 {
		t.Fatalf("memory.vote entries = %d, want 0 - the plan judge never votes", votes)
	}
}

// A plan declaring setup is known to belong to a repo, so recall must query the repo bucket: a memory
// with only a repo key must still surface.
func TestNewPlanJudge_RepoScopeRecall_NotUserOnly(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStoreForVoteTest(t)
	const repoKey = "github.com/acme/widgets"
	if _, err := store.Commit(ctx, memory.Scope{Repo: repoKey}, "seed", memory.Provenance{},
		[]memory.Candidate{{Content: "this repo requires conventional commits"}}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var prompt string
	judge := NewPlanJudge(planPromptCapturingModel(&prompt), 0, "", store, ledgertest.NewMemStore())

	runCtx := ledger.WithCoords(ctx, ledger.Coords{ChatID: "chat-repo"})
	accept, _, err := judge(runCtx, "review the PR", "1 node(s):\n- review (code-reviewer)", repoKey)
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if !accept {
		t.Fatal("want accept")
	}
	if !strings.Contains(prompt, "conventional commits") {
		t.Fatalf("prompt missing the repo-scoped memory - recall fell back to user-only scope: %q", prompt)
	}
}

func TestPlanJudgeAccepts(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel(true, ""), 0, "", nil, nil)
	accept, reason, err := judge(context.Background(), "write a plan", "1 node(s):\n- explore (web-researcher)", "")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if !accept {
		t.Errorf("accept = false, want true (reason %q)", reason)
	}
}

func TestPlanJudgeRejectsWithReason(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel(false, "add a code-implementer node"), 0, "", nil, nil)
	accept, reason, err := judge(context.Background(), "implement and ship it", "1 node(s):\n- explore (web-researcher)", "")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if accept {
		t.Error("accept = true, want false")
	}
	if reason != "add a code-implementer node" {
		t.Errorf("reason = %q, want %q", reason, "add a code-implementer node")
	}
}

func TestPlanJudgeErrorsWithoutVerdict(t *testing.T) {
	judge := NewPlanJudge(noVerdictModel, 0, "", nil, nil)
	if _, _, err := judge(context.Background(), "x", "y", ""); err == nil {
		t.Fatal("PlanJudge: expected an error when the model never calls submit_plan_verdict")
	}
}

// The plan judge never crosses a RunNode boundary, so a ChatID on the caller's ctx must reach its own
// "chat" ledger event, not fall back to "unscoped".
func TestPlanJudge_ChatEventCarriesCallerCoords(t *testing.T) {
	capExp := &captureEvalExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	traced := inference.TracedModelForTesting(stubPlanJudgeModel(true, ""), "plan-judge-test-model")
	judge := NewPlanJudge(traced, 0, "", nil, nil)

	const chatID = "planner-chat"
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID})
	if _, _, err := judge(ctx, "write a plan", "1 node(s):\n- explore (web-researcher)", ""); err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}

	var gotChatID string
	var found bool
	for _, r := range capExp.records {
		var operation string
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			switch string(kv.Key) {
			case otelobs.GenAIOperationName:
				operation = kv.Value.AsString()
			case otelobs.GenAIConversationID:
				gotChatID = kv.Value.AsString()
			}
			return true
		})
		if operation == otelobs.GenAIOperationChat {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no chat ledger event recorded for the plan judge's call")
	}
	if gotChatID != chatID {
		t.Errorf("plan judge chat gen_ai.conversation.id = %q, want %q", gotChatID, chatID)
	}
}

// A small cohesive change with setup and delivery declared is a one-node plan and must be accepted,
// not forced into an API/logic/tests chain.
func TestPlanJudgeAcceptsCohesiveSingleNodePlan(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel(true, ""), 0, "", nil, nil)
	planSummary := "1 node(s):\n" +
		"- implement (code-implementer): add a 👀 reaction to the API, implement the logic, write tests, and run checks\n" +
		"setup: repo=github.com/example/app work_branch=feat/eyes-reaction\n" +
		"delivery: kind=pull_request"
	accept, reason, err := judge(context.Background(), "add a 👀-reaction feature", planSummary, "")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if !accept {
		t.Errorf("accept = false, want true for a cohesive single-node plan (reason %q)", reason)
	}
}

// The rubric must tell the judge that a cohesive change stays one node rather than slicing by
// activity (API / logic / tests); asserted on the text so it can't regress silently.
func TestPlanRubricCriterion5RequiresSingleNodeForCohesiveWork(t *testing.T) {
	mustContain := []string{
		"decompose by independent PORTION of work",
		"NEVER by activity or layer",
		"is CORRECTLY a SINGLE code-implementer node",
		"only when the request genuinely contains multiple INDEPENDENT portions",
	}
	for _, s := range mustContain {
		if !strings.Contains(planRubricInstruction, s) {
			t.Errorf("criterion 5 rubric text missing expected phrase: %q", s)
		}
	}
}

// The rubric names the activity split (API / logic / tests / checks / commit) as a failure.
func TestPlanRubricCriterion5ForbidsActivitySplit(t *testing.T) {
	if !strings.Contains(planRubricInstruction, `splitting "API implementation" vs. "logic implementation" vs. "testing/verification" vs. "run checks" vs. "commit" into separate nodes for what is really one goal FAILS this criterion`) {
		t.Error("criterion 5 rubric text must explicitly forbid splitting one cohesive goal into API/logic/tests/checks/commit nodes")
	}
}

// A request that narrows scope (a commit, threads, files) is judged against that scope: a small plan
// for a narrow ask passes, inflating it into the large-diff fan-out fails.
func TestPlanRubricRequiresAskScopeFidelity(t *testing.T) {
	mustContain := []string{
		"is sized to the SCOPE the request itself sets",
		"the plan must match THAT scope, not the whole PR/repo",
		"never a license to expand past what was asked",
		"an explicitly narrowed ask forced into the large-diff fan-out pattern",
	}
	for _, s := range mustContain {
		if !strings.Contains(planRubricInstruction, s) {
			t.Errorf("ask-scope-fidelity rubric text missing expected phrase: %q", s)
		}
	}
}

// Criterion 1 asks whether the terminal node produces the artifact the request asked for, reasoned
// from the request. Deliberately loose: only the rule must survive, not the prose.
func TestPlanRubricRequiresRequestArtifactMatch(t *testing.T) {
	mustContain := []string{
		"does it hand back what the request asked to receive",
		"terminal",
		"never satisfies a request whose deliverable is a plan, a review, or shipped code",
	}
	for _, s := range mustContain {
		if !strings.Contains(planRubricInstruction, s) {
			t.Errorf("request-artifact-match rubric text missing expected phrase: %q", s)
		}
	}
}

// A plan-only request whose single terminal node is a code-explorer report must be rejected with a
// reason naming the missing plan-producing node. Pins the plumbing, not the model's judgment.
func TestPlanJudgeRejectsExplorationTerminalForPlanRequest(t *testing.T) {
	reason := "add a terminal node that actually writes the plan - the current terminal node only explores and produces a report"
	judge := NewPlanJudge(stubPlanJudgeModel(false, reason), 0, "", nil, nil)
	planSummary := "1 node(s):\n" +
		"- explore (code-explorer): Explore the repository and produce a detailed report covering: files, Compose patterns, Gradle config, navigation\n" +
		"delivery: kind=comment"
	accept, gotReason, err := judge(context.Background(),
		"Produce an implementation plan for issue #63: lay out a concrete plan - the approach, the files to change, and how to verify it.",
		planSummary, "")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if accept {
		t.Error("accept = true, want false: a bare code-explorer terminal node tasked to produce a report does not satisfy a request for an implementation plan")
	}
	if !strings.Contains(gotReason, "terminal") {
		t.Errorf("reason = %q, want it to name the missing plan-producing terminal node", gotReason)
	}
}

// Mirror case: a plan that stops at exploration does not satisfy a request for shipped code either.
func TestPlanJudgeRejectsExplorationTerminalForImplementRequest(t *testing.T) {
	reason := "this plan stops at exploration; add a terminal code-implementer node that ships the change"
	judge := NewPlanJudge(stubPlanJudgeModel(false, reason), 0, "", nil, nil)
	planSummary := "1 node(s):\n" +
		"- explore (code-explorer): Explore the repository and report where the dark-mode toggle should be added\n"
	accept, gotReason, err := judge(context.Background(),
		"Add a dark-mode toggle to the settings screen and open a pull request.",
		planSummary, "")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if accept {
		t.Error("accept = true, want false: a plan that only explores does not satisfy a request to ship code")
	}
	if !strings.Contains(gotReason, "terminal") {
		t.Errorf("reason = %q, want it to name the missing terminal code-implementer node", gotReason)
	}
}

// A plan with no delivery declared is a legitimate partial step: the judge asks whether THIS step
// makes progress given what already ran, not whether the plan is finished.
func TestPlanRubricStatesProgressFramingForPartialPlans(t *testing.T) {
	mustContain := []string{
		"A plan is not required to be complete",
		"built incrementally",
		"Makes progress toward what was asked, given the whole conversation and what already ran",
		"A plan with no delivery declared is a STATEMENT that more work follows, not a defect",
		"only require that this step is a genuine, non-redundant move toward that eventual terminal node",
	}
	for _, s := range mustContain {
		if !strings.Contains(planRubricInstruction, s) {
			t.Errorf("progress-framing rubric text missing expected phrase: %q", s)
		}
	}
}

func TestPlanRubricStatesTerminalOutputIsTheAnswer(t *testing.T) {
	r := planRubricInstruction
	for _, want := range []string{
		"TERMINAL node's own output IS what the user receives",
		"Nothing runs after the graph",
		"final response",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("plan rubric is missing %q - the judge needs the mechanism stated, not implied", want)
		}
	}
}

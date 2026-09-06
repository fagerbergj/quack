package vetting

import (
	"context"
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
	"github.com/fagerbergj/quack/internal/otelobs"
)

// stubPlanJudgeModel is a scripted model.LLM that always calls
// submit_plan_verdict with the canned accept/reason - proving NewPlanJudge's
// tool wiring (submit tool built, run isolated, sink read back) without a live
// model.
type stubPlanJudgeModel struct {
	accept bool
	reason string
}

func (stubPlanJudgeModel) Name() string { return "stub-plan-judge" }

func (s stubPlanJudgeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(stubCall(submitPlanVerdictTool, map[string]any{"accept": s.accept, "reason": s.reason}), nil)
	}
}

// noVerdictModel ends its run without ever calling submit_plan_verdict -
// exercises NewPlanJudge's "judge ended without a verdict" error path.
type noVerdictModel struct{}

func (noVerdictModel) Name() string { return "no-verdict" }

func (noVerdictModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{TurnComplete: true}, nil)
	}
}

// maxTokensRecordingPlanJudge records the MaxOutputTokens the request
// actually carried before accepting - proving the shared judge/
// plan-judge cap reaches the plan judge's own model request too (#889).
type maxTokensRecordingPlanJudge struct{ got *int32 }

func (maxTokensRecordingPlanJudge) Name() string { return "max-tokens-recording-plan-judge" }

func (m maxTokensRecordingPlanJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if req.Config != nil {
			atomic.StoreInt32(m.got, req.Config.MaxOutputTokens)
		}
		yield(stubCall(submitPlanVerdictTool, map[string]any{"accept": true, "reason": ""}), nil)
	}
}

// TestPlanJudge_RequestCarriesConfiguredMaxOutputTokens proves the cap
// argument reaches the plan judge's own model request.
func TestPlanJudge_RequestCarriesConfiguredMaxOutputTokens(t *testing.T) {
	got := int32(-1)
	judge := NewPlanJudge(maxTokensRecordingPlanJudge{got: &got}, 1024)
	if _, _, err := judge(context.Background(), "write a plan", "1 node(s):\n- explore (web-researcher)"); err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if got != 1024 {
		t.Errorf("req.Config.MaxOutputTokens = %d, want 1024", got)
	}
}

// loopingPlanJudgeModel streams the same short phrase as many small partial
// deltas within ONE model call, never calling submit_plan_verdict - the
// literal #889 incident shape (a single generation that decodes forever
// instead of stopping), since the plan judge has no other tool available to
// sustain a multi-turn loop across separate model calls the way the judge
// tests do with read_file.
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

// TestPlanJudge_RunawayRepeatRoutesToNoVerdict proves #889's repeat guard on
// the plan judge: a runaway streaming loop is aborted well before the model's
// own (bounded, in this fake) 500-chunk script would otherwise finish, and
// the round ends the same "ended without a verdict" way an ordinary no-tool-
// call reply does.
func TestPlanJudge_RunawayRepeatRoutesToNoVerdict(t *testing.T) {
	m := &loopingPlanJudgeModel{}
	judge := NewPlanJudge(m, 0)
	_, _, err := judge(context.Background(), "write a plan", "1 node(s):\n- explore (web-researcher)")
	if err == nil {
		t.Fatal("PlanJudge: expected an error - the model never calls submit_plan_verdict")
	}
	if got := atomic.LoadInt32(&m.chunks); got >= 500 {
		t.Errorf("model streamed %d chunks, want fewer than the scripted 500 - the repeat guard should have aborted early", got)
	}
}

func TestPlanJudgeAccepts(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel{accept: true, reason: ""}, 0)
	accept, reason, err := judge(context.Background(), "write a plan", "1 node(s):\n- explore (web-researcher)")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if !accept {
		t.Errorf("accept = false, want true (reason %q)", reason)
	}
}

func TestPlanJudgeRejectsWithReason(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel{accept: false, reason: "add a code-implementer node"}, 0)
	accept, reason, err := judge(context.Background(), "implement and ship it", "1 node(s):\n- explore (web-researcher)")
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
	judge := NewPlanJudge(noVerdictModel{}, 0)
	if _, _, err := judge(context.Background(), "x", "y"); err == nil {
		t.Fatal("PlanJudge: expected an error when the model never calls submit_plan_verdict")
	}
}

// TestPlanJudge_ChatEventCarriesCallerCoords pins #617's planner entry point:
// the plan judge runs its own isolated agent.Run, but never crosses a
// workflow.RunNode boundary - a ChatID stamped on the caller's ctx (as
// tools.NewPlanTool's handler now does) must reach the judge's OWN "chat"
// ledger event, not fall back to "unscoped".
func TestPlanJudge_ChatEventCarriesCallerCoords(t *testing.T) {
	capExp := &captureEvalExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	traced := inference.TracedModelForTesting(stubPlanJudgeModel{accept: true}, "plan-judge-test-model")
	judge := NewPlanJudge(traced, 0)

	const chatID = "planner-chat"
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID})
	if _, _, err := judge(ctx, "write a plan", "1 node(s):\n- explore (web-researcher)"); err != nil {
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

// TestPlanJudgeAcceptsCohesiveSingleNodePlan pins the plumbing for the case
// that motivated the criterion-7 reword: a small, cohesive change (add one
// reaction to an API) with setup + delivery declared is a ONE-node plan, and
// must be accepted rather than forced into an API/logic/tests chain.
func TestPlanJudgeAcceptsCohesiveSingleNodePlan(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel{accept: true, reason: ""}, 0)
	planSummary := "1 node(s):\n" +
		"- implement (code-implementer): add a 👀 reaction to the API, implement the logic, write tests, and run checks\n" +
		"setup: repo=github.com/example/app work_branch=feat/eyes-reaction\n" +
		"delivery: kind=pull_request"
	accept, reason, err := judge(context.Background(), "add a 👀-reaction feature", planSummary)
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if !accept {
		t.Errorf("accept = false, want true for a cohesive single-node plan (reason %q)", reason)
	}
}

// TestPlanRubricCriterion5RequiresSingleNodeForCohesiveWork pins the reworded
// rubric text against the live over-decomposition bug: the judge rejected a
// correct single-node plan for splitting "into separate nodes for API
// implementation, logic implementation, and testing/verification" - activity
// slicing, not independent-portion slicing. This asserts the instruction text
// itself so the guidance can't silently regress without a model run.
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

// TestPlanRubricCriterion5ForbidsActivitySplit pins that the rubric names the
// exact activity split the maintainer observed (API / logic / tests / checks
// / commit) as a FAILURE, not a pass - the bug this fix closes.
func TestPlanRubricCriterion5ForbidsActivitySplit(t *testing.T) {
	if !strings.Contains(planRubricInstruction, `splitting "API implementation" vs. "logic implementation" vs. "testing/verification" vs. "run checks" vs. "commit" into separate nodes for what is really one goal FAILS this criterion`) {
		t.Error("criterion 5 rubric text must explicitly forbid splitting one cohesive goal into API/logic/tests/checks/commit nodes")
	}
}

// TestPlanRubricRequiresAskScopeFidelity pins the PR-607 fix: a request that
// explicitly narrows scope (a specific commit, threads, files) must be judged
// against THAT scope, not the whole PR/repo - so a plan honoring the narrow
// ask with one small node is not rejected for looking small next to a large
// diff, and a plan that inflates a narrow ask into the large-diff fan-out
// pattern is rejected on this criterion.
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

// TestPlanRubricRequiresRequestArtifactMatch pins the #634 fix: criterion 1
// collapsed the old request-type enumeration (which licensed an
// explorer-shaped "plan") into one generic check - does the plan's TERMINAL
// node actually produce the artifact the request asked to receive, reasoned
// from the request itself rather than pattern-matched against a catalogue of
// request-type shapes. Deliberately loose: the exact prose is free to evolve,
// only the rule needs to survive.
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

// TestPlanJudgeRejectsExplorationTerminalForPlanRequest pins the #634 shape
// itself: a plan-only request ("produce an implementation plan: the
// approach, the files to change, and how to verify it") whose single
// terminal node is a code-explorer tasked to "produce a detailed report"
// must be rejected, with a reason naming the missing plan-producing terminal
// node - not accepted as a correct "plan-only, stays read-only" shape. The
// judge's own reasoning runs against a live model; this pins the plumbing
// (the judge propagates a reject verdict + reason for this exact shape)
// rather than the model's judgment, which is untestable without one.
func TestPlanJudgeRejectsExplorationTerminalForPlanRequest(t *testing.T) {
	reason := "add a terminal node that actually writes the plan - the current terminal node only explores and produces a report"
	judge := NewPlanJudge(stubPlanJudgeModel{accept: false, reason: reason}, 0)
	planSummary := "1 node(s):\n" +
		"- explore (code-explorer): Explore the repository and produce a detailed report covering: files, Compose patterns, Gradle config, navigation\n" +
		"delivery: kind=comment"
	accept, gotReason, err := judge(context.Background(),
		"Produce an implementation plan for issue #63: lay out a concrete plan - the approach, the files to change, and how to verify it.",
		planSummary)
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

// TestPlanJudgeRejectsExplorationTerminalForImplementRequest is the mirror
// of the #634 shape: a plan that stops at exploration does not satisfy a
// request whose deliverable is shipped code either.
func TestPlanJudgeRejectsExplorationTerminalForImplementRequest(t *testing.T) {
	reason := "this plan stops at exploration; add a terminal code-implementer node that ships the change"
	judge := NewPlanJudge(stubPlanJudgeModel{accept: false, reason: reason}, 0)
	planSummary := "1 node(s):\n" +
		"- explore (code-explorer): Explore the repository and report where the dark-mode toggle should be added\n"
	accept, gotReason, err := judge(context.Background(),
		"Add a dark-mode toggle to the settings screen and open a pull request.",
		planSummary)
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

// TestPlanRubricStatesTerminalOutputIsTheAnswer pins the architectural fact the
// judge twice confabulated away: it accepted a lone code-explorer plan because
// the findings "will be used to write the plan in the final response". No such
// step exists - buildPlanGraph enforces one terminal node and its output is
// delivered verbatim - so the rubric has to say so.
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

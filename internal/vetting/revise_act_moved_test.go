package vetting

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	workflowagent "google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// actMovedReviseModel: revise round ends on an empty answer (finalSpec answer_len
// 0), same as emptyReviseModel, but its LAST act before that empty text is a git_commit tool call - so the delivered activity changed even though the
// answer bytes didn't. The first judge round fails; the tool call should fix exactly what it complained about, so a second judge round must see it.
type actMovedReviseModel struct {
	workerCalls int
	judgeCalls  int
}

func (m *actMovedReviseModel) Name() string { return "stub-act-moved-revise" }

func (m *actMovedReviseModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			m.judgeCalls++
			if m.judgeCalls == 1 {
				yield(stubCall(submitVerdictTool, map[string]any{"score": 0.4, "feedback": "commit the fix"}), nil)
			} else {
				yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": "committed, looks good"}), nil)
			}
			return
		}
		m.workerCalls++
		text := stubAllText(req)
		switch {
		case !strings.Contains(text, "Verdict:"):
			yield(stubText("This is the initial draft answer, long enough to be judged."), nil)
		case stubHasResponse(req, "git_commit"):
			yield(stubText(""), nil) // revise round ends on the tool call, no trailing text
		default:
			yield(stubCall("git_commit", map[string]any{"message": "address feedback"}), nil)
		}
	}
}

// TestReviseActMovedForcesRejudge proves that an empty revise answer whose
// round DID stage new activity (here, a git_commit) still gets judged before
// delivery - node.go's revise guard must compare activity, not just answer text, or a committed fix goes out under the stale pre-commit verdict.
func TestReviseActMovedForcesRejudge(t *testing.T) {
	stub := &actMovedReviseModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "implementer",
		Instruction: "Do the task.", Tools: []tool.Tool{commitTool(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10"}
	var res GateResult
	node, err := newTestGatedNodeCapture("impl-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg, &res)
	if err != nil {
		t.Fatal(err)
	}
	root, err := workflowagent.New(workflowagent.Config{Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Fix the bug and commit it."}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Round 1 fails, revise commits then returns empty text: the loop must NOT
	// stop there (the committed fix was never judged) - it must judge again.
	if stub.judgeCalls != 2 {
		t.Fatalf("judge calls = %d, want 2 (round 1 fail, revise commits, round 2 must judge the commit)", stub.judgeCalls)
	}
	if !res.Passed || res.Score != 0.9 {
		t.Fatalf("result = %+v, want the round-2 verdict (passed, score 0.9) - not the stale round-1 fail delivered alongside the new commit", res)
	}
	if res.Rounds != 2 {
		t.Fatalf("rounds = %d, want 2", res.Rounds)
	}
}

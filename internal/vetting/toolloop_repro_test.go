package vetting

import (
	"context"
	"iter"
	"testing"

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

type probeArgs struct {
	Q string `json:"q"`
}
type probeResult struct {
	Out string `json:"out"`
}

// TestWorkerSeesToolError is the direct test of "are ERRORS forwarded?": the
// probe tool returns a Go error; the worker's follow-up call must carry an error
// FunctionResponse so the model can adapt (not re-issue the same failing call).
func TestWorkerSeesToolError(t *testing.T) {
	stub := &toolLoopStub{}
	probe, err := functiontool.New[probeArgs, probeResult](
		functiontool.Config{Name: "probe", Description: "probe the environment"},
		func(_ adkagent.Context, _ probeArgs) (probeResult, error) {
			return probeResult{}, context.DeadlineExceeded // any error: does it reach the model?
		})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "impl",
		Instruction: "Do the task.", Tools: []tool.Tool{probe},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10"}
	node, err := newTestGatedNode("impl-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatal(err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "probe then finish"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	t.Logf("ERROR-variant: workerCalls=%d sawProbeResult=%v probeRole=%q", stub.workerCalls, stub.sawProbeResult, stub.probeRole)
	if !stub.sawProbeResult {
		t.Fatalf("REPRO: worker never saw the tool ERROR - a failed tool call is not forwarded, so the model can't adapt and re-issues it. workerCalls=%d", stub.workerCalls)
	}
}

// toolLoopStub drives ONE worker tool call and records whether the worker's
// FOLLOW-UP model call actually carried the tool RESULT back (and under what
// role). This is the empirical test of "are tool results forwarded to the
// task-mode worker, or ejected/lost so it re-issues the same call forever".
type toolLoopStub struct {
	workerCalls    int
	sawProbeResult bool
	probeRole      string
}

func (m *toolLoopStub) Name() string { return "stub" }

func (m *toolLoopStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) { // judge: pass the draft immediately
			yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": "ok"}), nil)
			return
		}
		m.workerCalls++
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "probe" {
					m.sawProbeResult = true
					m.probeRole = c.Role
				}
			}
		}
		switch {
		case m.sawProbeResult || m.workerCalls >= 6: // saw it (good) or give up (avoid an infinite test loop)
			yield(stubText("Done."), nil)
		default:
			yield(stubCall("probe", map[string]any{"q": "where is go"}), nil)
		}
	}
}

// TestWorkerSeesItsOwnToolResult is the reproduction: a task-mode worker (built
// exactly as production builds nodes - unset Mode, wrapped in a workflow node)
// makes one tool call; its follow-up model call MUST carry that tool's result,
// or the worker has amnesia and re-issues the same call forever (the #252 loop).
func TestWorkerSeesItsOwnToolResult(t *testing.T) {
	stub := &toolLoopStub{}
	probe, err := functiontool.New[probeArgs, probeResult](
		functiontool.Config{Name: "probe", Description: "probe the environment"},
		func(_ adkagent.Context, _ probeArgs) (probeResult, error) {
			return probeResult{Out: "PATH=/usr/bin; go: not found"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "impl",
		Instruction: "Do the task.", Tools: []tool.Tool{probe},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10"}
	node, err := newTestGatedNode("impl-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatal(err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "probe the env then finish"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	t.Logf("workerCalls=%d sawProbeResult=%v probeRole=%q", stub.workerCalls, stub.sawProbeResult, stub.probeRole)
	if !stub.sawProbeResult {
		t.Fatalf("REPRO: worker's follow-up model call did NOT carry the probe RESULT - tool results are not forwarded to the task-mode worker (amnesia loop). workerCalls=%d", stub.workerCalls)
	}
	if stub.probeRole != "user" {
		t.Errorf("probe result role = %q, want \"user\"; role \"model\" mislabels the tool result as the assistant's own words", stub.probeRole)
	}
}

package dag

import (
	"context"
	"iter"
	"sync"
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

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

const testAskTool = "ask_user"

// confirmationCallName is ADK's FunctionCall name for a HITL confirmation request
// (toolconfirmation.FunctionCallName). The resume FunctionResponse must echo it.
const confirmationCallName = "adk_request_confirmation"

// runPlanContent runs a plan through the single-runner path (execNode→RunPlanInNode)
// on ex's session store with the given content (a fresh user turn or a resume
// FunctionResponse), calling observe for every raw session event. Returns the
// captured node outputs. Reuses ex.sessions so a second call resumes the first.
func runPlanContent(t *testing.T, ex *Executor, plan Plan, chatID string, content *genai.Content, observe func(*session.Event)) (map[string]string, error) {
	t.Helper()
	var planOutputs map[string]string
	dsOutputs := map[string]string{}
	yield := func(stream.SSEEvent, error) bool { return true }
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			o, err := ex.RunPlanInNode(ctx, plan, chatID)
			planOutputs = o
			return "done", err
		}, workflow.NodeConfig{})
	top, err := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	if err != nil {
		return nil, err
	}
	r, err := runner.New(runner.Config{AppName: "quack", Agent: top, SessionService: ex.sessions, AutoCreateSession: true})
	if err != nil {
		return nil, err
	}
	ctx := stream.WithYield(context.Background(), func(ev stream.SSEEvent) { yield(ev, nil) })
	ds := ex.NewDagStream(ctx, plan, "quack", "u", chatID, chatID, yield, dsOutputs)
	for ev, rerr := range r.Run(ctx, "u", chatID, content, adkagent.RunConfig{}) {
		if rerr != nil {
			return nil, rerr
		}
		if ev == nil {
			continue
		}
		observe(ev)
		ds.Handle(ev)
	}
	ds.Finish()
	return planOutputs, nil
}

type askArgs struct {
	Question string `json:"question"`
}
type askResult struct {
	Status string `json:"status"`
}

// newAskTool is a minimal long-running "ask the user" tool (like get_user_choice
// but built inline to avoid the tools→dag import cycle).
func newAskTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[askArgs, askResult](
		functiontool.Config{Name: testAskTool, Description: "Ask the user a question."},
		func(tc adkagent.Context, a askArgs) (askResult, error) {
			// Native tool-level HITL: record a pending confirmation + halt the loop.
			if err := tc.RequestConfirmation(a.Question, nil); err != nil {
				return askResult{}, err
			}
			return askResult{Status: "pending"}, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return tl
}

// hitlStub: as a worker it calls the long-running ask_user tool (→ HITL pause); as
// a judge it passes.
type hitlStub struct {
	mu          sync.Mutex
	workerCalls int
}

func (*hitlStub) Name() string { return "hitlStub" }

func (s *hitlStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		s.workerCalls++
		s.mu.Unlock()
		// After the ask_user tool has returned (confirmation delivered), write the
		// answer; before that, ask the user.
		if gHasFuncResponse(req, testAskTool) {
			yield(gText("Chose the direction you picked. Final answer."), nil)
			return
		}
		yield(gCall(testAskTool, map[string]any{"question": "which direction?"}), nil)
	}
}

// gHasFuncResponse reports whether the request's contents carry a FunctionResponse
// for the named tool (i.e. the tool has already run and returned).
func gHasFuncResponse(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return true
			}
		}
	}
	return false
}

// TestHITL_WorkerPausesNode: a worker that calls a long-running tool parks its node
// instead of failing — WithRaiseOnWait turns the unresolved tool into an interrupt
// that runDAG→execNode swallows into a pause. The node emits no answer and the tool
// call surfaces to the client.
func TestHITL_WorkerPausesNode(t *testing.T) {
	stub := &hitlStub{}
	ag, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{newAskTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag}, nil, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}

	events, outputs := runPlanSSE(t, ex, plan, "s")

	if out := outputs["n1"]; out != "" {
		t.Errorf("paused node should have no output, got %q", out)
	}
	var sawAsk, sawDone bool
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case stream.AgentToolCallData:
			if d.Name == testAskTool {
				sawAsk = true
			}
		case stream.NodeDoneData:
			if d.NodeID == "n1" {
				sawDone = true
			}
		}
	}
	if !sawAsk {
		t.Errorf("expected the ask_user tool call to surface in events")
	}
	if sawDone {
		t.Errorf("node n1 should be paused, not done")
	}
	if stub.workerCalls == 0 {
		t.Errorf("worker never ran")
	}
}

// TestHITL_WorkerResumeReAsks documents the OPEN resume problem (Path B, B1 variant):
// a worker parking on tc.RequestConfirmation cannot be resumed by delivering the
// confirmation FunctionResponse, because runDAG re-runs the whole plan on resume
// (RerunOnResume) and the worker llmagent re-executes with FRESH tool-call IDs — so
// the confirmation, tied to run-1's call ID, never matches the run-2 worker, which
// just asks again and re-parks.
//
// The fix is B2: the GATE NODE BODY (not the worker llmagent) owns the pause via
// workflow.ResumeOrRequestInput with a STABLE, node-based InterruptID (the spike
// TestPathB_RunDAGNestingHITLResume proved that resumes), detecting the worker's
// intent to ask and feeding the answer back into the worker's next run. Until B2 is
// built, this test asserts the current re-ask behavior so the regression is visible.
func TestHITL_WorkerResumeReAsks(t *testing.T) {
	stub := &hitlStub{}
	ag, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{newAskTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := NewExecutor(sessions, map[string]adkagent.Agent{"blk": ag}, nil, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}

	confirmID := ""
	capture := func(ev *session.Event) {
		if ev == nil || ev.Content == nil {
			return
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.Name == confirmationCallName {
				confirmID = p.FunctionCall.ID
			}
		}
	}
	out1, err := runPlanContent(t, ex, plan, "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, capture)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if out1["n1"] != "" {
		t.Fatalf("run1: node should be paused, got %q", out1["n1"])
	}
	if confirmID == "" {
		t.Fatal("run1: never saw an adk_request_confirmation call")
	}

	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: confirmID, Name: confirmationCallName,
			Response: map[string]any{"confirmed": true, "payload": "north"},
		},
	}}}
	out2, err := runPlanContent(t, ex, plan, "s", answer, func(*session.Event) {})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	// KNOWN LIMITATION: resume does NOT complete the node (it re-asks). When B2 lands,
	// flip this to require out2["n1"] != "" and rename the test.
	if out2["n1"] != "" {
		t.Errorf("resume now COMPLETES the node (out=%q) — B2 may be done; update this test to assert success", out2["n1"])
	}
}

package dag

import (
	"context"
	"iter"
	"strings"
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

type askArgs struct {
	Question string `json:"question"`
}
type askResult struct {
	Status string `json:"status"`
}

// newAskTool mirrors tools.NewAskUserTool: a plain tool that records the question
// (in its call args) and ends the worker's turn; the GATE detects the call and
// pauses the node. Built inline to avoid the tools→dag import cycle.
func newAskTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[askArgs, askResult](
		functiontool.Config{Name: vetting.AskToolName, Description: "Ask the user a question."},
		func(tc adkagent.Context, _ askArgs) (askResult, error) {
			tc.Actions().SkipSummarization = true
			return askResult{Status: "forwarded to the user"}, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return tl
}

// hitlStub: as a judge it passes; as a worker it asks the user via ask_user unless
// its request already carries the delivered answer (the gate's withUserAnswer
// prompt), in which case it writes the final answer.
type hitlStub struct {
	mu          sync.Mutex
	workerCalls int
	sawAnswer   string // the user answer text observed in the post-answer prompt
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
		if txt := gUserText(req); strings.Contains(txt, "they have now answered") {
			// Post-answer run: extract the answer line for assertions.
			if i := strings.Index(txt, "\nA: "); i >= 0 {
				line := txt[i+4:]
				if j := strings.IndexByte(line, '\n'); j >= 0 {
					line = line[:j]
				}
				s.mu.Lock()
				s.sawAnswer = line
				s.mu.Unlock()
			}
			yield(gText("Final answer using the user's direction."), nil)
			return
		}
		yield(gCall(vetting.AskToolName, map[string]any{"question": "which direction?"}), nil)
	}
}

func newHITLExecutor(t *testing.T, stub *hitlStub, sessions session.Service) (*Executor, Plan) {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{newAskTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(sessions, map[string]adkagent.Agent{"blk": ag}, nil, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}
	return ex, plan
}

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

// TestHITL_GatePausesAndResumes proves the full mid-node HITL cycle through the
// real gate: the worker calls ask_user → the gate parks the NODE under the stable
// interrupt ID hitl-n1-r1 (run 1 ends, no node output); delivering the user's
// answer as the adk_request_input FunctionResponse re-enters the node, the gate
// re-runs the worker with the Q&A folded in, and the node completes.
func TestHITL_GatePausesAndResumes(t *testing.T) {
	stub := &hitlStub{}
	ex, plan := newHITLExecutor(t, stub, session.InMemoryService())

	// Run 1: park. Capture the pause request from the raw events.
	var pauseID, pauseMsg string
	out1, err := runPlanContent(t, ex, plan, "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, func(ev *session.Event) {
		if ev != nil && ev.RequestedInput != nil {
			pauseID = ev.RequestedInput.InterruptID
			pauseMsg = ev.RequestedInput.Message
		}
	})
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if out1["n1"] != "" {
		t.Fatalf("run1: paused node should have no output, got %q", out1["n1"])
	}
	if pauseID != "hitl-n1-r1" {
		t.Fatalf("run1: interrupt ID = %q, want hitl-n1-r1", pauseID)
	}
	if pauseMsg != "which direction?" {
		t.Errorf("run1: pause message = %q, want the worker's question", pauseMsg)
	}

	// Run 2: deliver the answer.
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: pauseID, Name: workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": "north"},
		},
	}}}
	out2, err := runPlanContent(t, ex, plan, "s", answer, func(*session.Event) {})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if out2["n1"] == "" {
		t.Fatalf("run2: node should complete after resume, got none")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.sawAnswer != "north" {
		t.Errorf("worker never received the user's answer: sawAnswer=%q", stub.sawAnswer)
	}
}

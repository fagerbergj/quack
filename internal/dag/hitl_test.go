package dag

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
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

// TestHITL_SingleNodePauseResume covers the degenerate plan where the ASKER is
// itself the terminal node (no synthesizer): run 1 parks it under hitl-n1-r1; the
// answer turn re-enters it and its output becomes the plan's terminal answer.
func TestHITL_SingleNodePauseResume(t *testing.T) {
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

	var pauseID, pauseMsg string
	yield := func(ev stream.SSEEvent, _ error) bool {
		if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
			pauseID, pauseMsg = d.InterruptID, d.Message
		}
		return true
	}
	out1 := map[string]string{}
	paused, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "s",
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, yield, out1, nil)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if !paused || out1["n1"] != "" {
		t.Fatalf("run1: want paused with no output, got paused=%v out=%q", paused, out1["n1"])
	}
	if pauseID != "hitl-n1-r1" || pauseMsg != "which direction?" {
		t.Fatalf("run1: node_needs_input = (%q, %q), want (hitl-n1-r1, which direction?)", pauseID, pauseMsg)
	}

	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: pauseID, Name: workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": "north"},
		},
	}}}
	out2 := map[string]string{}
	paused2, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "s", answer, yield, out2, []string{"n1"})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if paused2 || out2["n1"] == "" {
		t.Fatalf("run2: want completed with output, got paused=%v out=%q", paused2, out2["n1"])
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.sawAnswer != "north" {
		t.Errorf("worker never received the user's answer: sawAnswer=%q", stub.sawAnswer)
	}
}

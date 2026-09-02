package vetting

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/agent"
)

const steerMarker = "STOP-RESEARCHING-AND-ANSWER"

// steerCtrl arms itself only once the worker round is already under way, so
// TakeQueued reproduces a user steering a RUNNING node rather than a message
// that was already parked before the round started.
type steerCtrl struct {
	mu    sync.Mutex
	armed bool
	taken bool
}

func (c *steerCtrl) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
}

func (c *steerCtrl) Cancelled() bool { return false }
func (c *steerCtrl) Paused() bool    { return false }
func (c *steerCtrl) TakeQueued() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed || c.taken {
		return ""
	}
	c.taken = true
	return steerMarker
}
func (c *steerCtrl) PauseForInput(string) {}

// steerStub drives one worker round with TWO model calls: call 1 asks for a
// tool, call 2 answers. The steer is queued between them.
type steerStub struct {
	mu       sync.Mutex
	ctrl     *steerCtrl
	workerN  int
	requests []string
}

func (s *steerStub) Name() string { return "steer-stub" }

func (s *steerStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			yield(stubCall(submitVerdictTool, map[string]any{
				"criteria": map[string]any{"quality": map[string]any{"score": 1.0, "reason": "fine"}},
			}), nil)
			return
		}
		s.mu.Lock()
		s.workerN++
		n := s.workerN
		s.requests = append(s.requests, requestText(req))
		s.mu.Unlock()

		if n == 1 {
			s.ctrl.arm() // the user steers now, mid-round
			yield(stubCall("look_up", map[string]any{"topic": "anything"}), nil)
			return
		}
		yield(stubText("ANSWER"), nil)
	}
}

func requestText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil {
				b.WriteString(p.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

func newLookUpTool(t *testing.T) tool.Tool {
	t.Helper()
	type args struct {
		Topic string `json:"topic"`
	}
	tl, err := functiontool.New[args, map[string]any](
		functiontool.Config{Name: "look_up", Description: "stub lookup"},
		func(_ adkagent.Context, a args) (map[string]any, error) {
			return map[string]any{"result": "some findings"}, nil
		},
	)
	if err != nil {
		t.Fatalf("look_up tool: %v", err)
	}
	return tl
}

// #1029: a steer queued while a NATIVE (non-ACP) node is running must reach
// the model. Live steering is registered only on the ACP path, so for the four
// native agents the message sits in the queue until the next gate boundary -
// minutes away, or never. It must land on the round's next model call.
func TestRunGatedRefine_SteerReachesRunningNativeNode(t *testing.T) {
	ctrl := &steerCtrl{}
	stub := &steerStub{ctrl: ctrl}

	// Built the way production builds a native worker (agent.Build), not with a
	// bare llmagent - the delivery hook lives on that path.
	worker, err := agent.Build(
		&agent.Bundle{Card: agent.Card{Name: "web-researcher", Description: "researcher"}, Prompt: "Answer the question."},
		stub, []tool.Tool{newLookUpTool(t)}, nil, agent.Compaction{}, "", nil, "", ctrl.TakeQueued)
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	workerNode, err := NewWorkerNode(worker)
	if err != nil {
		t.Fatalf("worker node: %v", err)
	}

	cfg := Config{JudgeRounds: 1, Threshold: 0.5, Rubric: "score it", ChatID: "chat-1", Agent: "web-researcher"}
	node := workflow.NewDynamicNode[string, string]("researcher-gate",
		func(ctx adkagent.Context, task string, emit func(*session.Event) error) (string, error) {
			answer, _, err := RunGatedRefine(ctx, "researcher-gate", workerNode, stub,
				NewJudgeFactory(stub, nil, nil), cfg, "research something", nil, ctrl, emit)
			return answer, err
		}, workflow.NodeConfig{})

	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker},
		Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "research something"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.requests) < 2 {
		t.Fatalf("worker round made %d model calls, need >=2 to steer between them", len(stub.requests))
	}
	if strings.Contains(stub.requests[0], steerMarker) {
		t.Fatalf("steer must not appear before it was sent (call 1)")
	}
	if !strings.Contains(stub.requests[1], steerMarker) {
		t.Errorf("steer queued mid-round never reached the model; call 2 input was:\n%s", stub.requests[1])
	}
}

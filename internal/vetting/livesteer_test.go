package vetting

import (
	"context"
	"errors"
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

const (
	steerMarker = "STOP-RESEARCHING-AND-ANSWER"
	steerTask   = "research something"
)

// steerCtrl models a user steering a running round: arm queues the message and, as dag's enqueue does
// for a native round, flags the round interrupted.
type steerCtrl struct {
	mu          sync.Mutex
	armed       bool
	taken       bool
	interrupted bool
	pauseToo    bool // the user paused the node as the steer landed
}

func (c *steerCtrl) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.interrupted = true, true
}

func (c *steerCtrl) Cancelled() bool { return false }
func (c *steerCtrl) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pauseToo && c.armed
}
func (c *steerCtrl) TakeQueued() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed || c.taken {
		return ""
	}
	c.taken = true
	return steerMarker
}
func (c *steerCtrl) SteerInterrupted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was := c.interrupted
	c.interrupted = false
	return was
}
func (c *steerCtrl) PauseForInput(string)          {}
func (c *steerCtrl) MarkDelivered()                {}
func (c *steerCtrl) RepeatFailure() (string, bool) { return "", false }
func (c *steerCtrl) ShuttingDown() bool            { return false }
func (c *steerCtrl) NoteDraft(string)              {}

// steerStub: worker call 1 asks for a tool and the user steers then, so call 2 dies with the aborted round;
// later calls answer. Every worker request's contents are kept.
type steerStub struct {
	mu       sync.Mutex
	ctrl     *steerCtrl
	requests [][]*genai.Content
}

func (s *steerStub) Name() string { return "steer-stub" }

func (s *steerStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if !stubHasTool(req, "look_up") { // the judge, including its tool-less closing turn
			if stubHasTool(req, submitVerdictTool) {
				yield(stubCall(submitVerdictTool, map[string]any{
					"criteria": map[string]any{"quality": map[string]any{"score": 1.0, "reason": "fine"}},
				}), nil)
				return
			}
			yield(stubText(`{"score": 3, "criteria": {"quality": {"score": 3, "reason": "fine"}}, "feedback": ""}`), nil)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, req.Contents)
		n := len(s.requests)
		s.mu.Unlock()

		switch n {
		case 1:
			s.ctrl.arm()
			yield(stubCall("look_up", map[string]any{"topic": "anything"}), nil)
		case 2: // the steer aborted the round: its next call dies with the round's context
			yield(nil, context.Canceled)
		default:
			yield(stubText("ANSWER"), nil)
		}
	}
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

// runSteeredNode runs one native gated node whose round the user steers, returning every worker request.
func runSteeredNode(t *testing.T, repair func(context.Context) bool) [][]*genai.Content {
	t.Helper()
	reqs, err := runSteeredGate(t, &steerCtrl{}, repair)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	return reqs
}

// runSteeredGate runs the gate once, returning its worker requests and its own error.
func runSteeredGate(t *testing.T, ctrl *steerCtrl, repair func(context.Context) bool) ([][]*genai.Content, error) {
	t.Helper()
	stub := &steerStub{ctrl: ctrl}
	worker, err := agent.Build(
		&agent.Bundle{Card: agent.Card{Name: "web-researcher", Description: "researcher"}, Prompt: "Answer the question."},
		nil, stub, []tool.Tool{newLookUpTool(t)}, nil, "", "", nil)
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	workerNode, err := NewWorkerNode(worker)
	if err != nil {
		t.Fatalf("worker node: %v", err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.5, Rubric: "score it", ChatID: "chat-1", Agent: "web-researcher", RepairSession: repair}
	var gateErr error
	node := workflow.NewDynamicNode[string, string]("researcher-gate",
		func(ctx adkagent.Context, _ string, emit func(*session.Event) error) (string, error) {
			answer, _, err := RunGatedRefine(ctx, "researcher-gate", workerNode, stub,
				NewJudgeFactory(stub, nil, nil), cfg, steerTask, nil, ctrl, emit)
			gateErr = err
			return answer, nil
		}, workflow.NodeConfig{})
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker},
		Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	for _, err := range r.Run(t.Context(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: steerTask}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.requests, gateErr
}

// lastUserText is the text of a request's final user turn that carries text.
func lastUserText(contents []*genai.Content) string {
	for i := len(contents) - 1; i >= 0; i-- {
		c := contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		var b strings.Builder
		for _, p := range c.Parts {
			if p != nil {
				b.WriteString(p.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}

// A steer that interrupts a native round reaches the model as the user's own next turn in the worker's
// session, not folded into a re-sent task.
func TestRunGatedRefine_SteerContinuesTheWorkerSession(t *testing.T) {
	reqs := runSteeredNode(t, func(context.Context) bool { return true })

	if got := lastUserText(reqs[len(reqs)-1]); !strings.Contains(got, steerMarker) || strings.Contains(got, steerTask) {
		t.Fatalf("last worker request's user turn = %q, want the steer alone", got)
	}
	if strings.Contains(lastUserText(reqs[0]), steerMarker) {
		t.Fatal("the steer reached the model before it was sent")
	}
}

// With no worker session to continue (an ACP node), the steer re-drafts with the task and message together.
func TestRunGatedRefine_SteerWithoutASessionRedraftsWithTheMessage(t *testing.T) {
	reqs := runSteeredNode(t, nil)

	if got := lastUserText(reqs[len(reqs)-1]); !strings.Contains(got, steerMarker) || !strings.Contains(got, "Queued user message") {
		t.Fatalf("last worker request's user turn = %q, want the task re-sent with the queued message", got)
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

// A pause landing with a steer parks the node with the message still queued, so the resume delivers it;
// taking it first would drain it into a run that parks and lose it.
func TestRunGatedRefine_PauseWithASteerLeavesItQueued(t *testing.T) {
	ctrl := &steerCtrl{pauseToo: true}
	_, err := runSteeredGate(t, ctrl, func(context.Context) bool { return true })
	if !errors.Is(err, ErrNodePaused) {
		t.Fatalf("gate err = %v, want ErrNodePaused", err)
	}
	if ctrl.taken {
		t.Error("the steer was drained into a run that parked")
	}
}

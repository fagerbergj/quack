package vetting

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
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// stopCtrl is a node control a test can stop; it has no round abort, so only the gate's own
// checks honor the stop.
type stopCtrl struct {
	mu                   sync.Mutex
	cancelled, delivered bool
	drafts               []string
	stopOnDraft          string
}

func (c *stopCtrl) Cancelled() bool               { c.mu.Lock(); defer c.mu.Unlock(); return c.cancelled }
func (c *stopCtrl) stop()                         { c.mu.Lock(); c.cancelled = true; c.mu.Unlock() }
func (c *stopCtrl) Paused() bool                  { return false }
func (c *stopCtrl) TakeQueued() string            { return "" }
func (c *stopCtrl) PauseForInput(string)          {}
func (c *stopCtrl) MarkDelivered()                { c.mu.Lock(); c.delivered = true; c.mu.Unlock() }
func (c *stopCtrl) RepeatFailure() (string, bool) { return "", false }
func (c *stopCtrl) ShuttingDown() bool            { return false }
func (c *stopCtrl) NoteDraft(d string) {
	c.mu.Lock()
	c.drafts = append(c.drafts, d)
	stop := d == c.stopOnDraft
	c.mu.Unlock()
	if stop {
		c.stop()
	}
}

// scriptStub: the worker answers from its script in order (repeating the last), the judge
// verdicts likewise; onJudge runs as each judge call starts.
type scriptStub struct {
	mu       sync.Mutex
	scores   []float64
	texts    []string
	onJudge  func()
	requests []string
}

func (s *scriptStub) Name() string { return "script" }

func (s *scriptStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		if stubHasTool(req, submitVerdictTool) {
			score := s.scores[0]
			if len(s.scores) > 1 {
				s.scores = s.scores[1:]
			}
			hook := s.onJudge
			s.mu.Unlock()
			if hook != nil {
				hook()
			}
			yield(stubCall(submitVerdictTool, map[string]any{"score": score, "feedback": "revise it"}), nil)
			return
		}
		s.requests = append(s.requests, requestText(req))
		text := s.texts[0]
		if len(s.texts) > 1 {
			s.texts = s.texts[1:]
		}
		s.mu.Unlock()
		yield(stubText(text), nil)
	}
}

// runGate drives RunGatedRefine for node n1 in a real runner over sess, returning its answer.
func runGate(t *testing.T, stub *scriptStub, ctrl NodeControl, sess session.Service, allowErr bool) string {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{Name: "n1", Model: stub, Description: "worker", Instruction: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	workerNode, err := NewWorkerNode(worker)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{ChatID: "chat1", Agent: "worker", NodeID: "n1", JudgeRounds: 1, Threshold: 0.7, Task: "answer it"}
	var answer string
	node := workflow.NewDynamicNode[string, string]("n1",
		func(c adkagent.Context, task string, emit func(*session.Event) error) (string, error) {
			a, _, err := RunGatedRefine(c, "n1", workerNode, stub, NewJudgeFactory(stub, nil, nil), cfg, task, nil, ctrl, emit)
			answer = a
			return a, err
		}, workflow.NodeConfig{})
	root, err := workflowagent.New(workflowagent.Config{Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: sess, AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range r.Run(t.Context(), "u", "s-n1", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil && !allowErr {
			t.Fatalf("run: %v", err)
		}
	}
	return answer
}

// TestRunGatedRefine_StopAfterRevisionKeepsRevision: a stop landing after a revision ends the
// node with that revision (recorded as its draft), not the pre-revision draft, and undelivered.
func TestRunGatedRefine_StopAfterRevisionKeepsRevision(t *testing.T) {
	stub := &scriptStub{texts: []string{"DRAFT", "REVISED"}, scores: []float64{0.1, 0.95}}
	ctrl := &stopCtrl{stopOnDraft: "REVISED"}
	if got := runGate(t, stub, ctrl, session.InMemoryService(), false); got != "REVISED" {
		t.Errorf("answer = %q, want the revision", got)
	}
	if ctrl.delivered || strings.Join(ctrl.drafts, ",") != "DRAFT,REVISED" {
		t.Errorf("delivered=%v drafts=%v, want undelivered with both drafts recorded", ctrl.delivered, ctrl.drafts)
	}
}

// TestRunGatedRefine_StopDuringPassingJudgeNotDelivered: a stop that lands while the judge
// runs, which then passes, is still honored before delivery.
func TestRunGatedRefine_StopDuringPassingJudgeNotDelivered(t *testing.T) {
	ctrl := &stopCtrl{}
	stub := &scriptStub{texts: []string{"DRAFT"}, scores: []float64{0.95}, onJudge: ctrl.stop}
	runGate(t, stub, ctrl, session.InMemoryService(), false)
	if ctrl.delivered {
		t.Error("a stopped node was delivered after its judge passed")
	}
}

// TestRunGatedRefine_WriterRecoverySeesOnlyOwnActivity: the tool-less writer's prompt lists
// this node's own activity, never a sibling's searches from the shared session.
func TestRunGatedRefine_WriterRecoverySeesOnlyOwnActivity(t *testing.T) {
	sess := session.InMemoryService()
	resp, err := sess.Create(t.Context(), &session.CreateRequest{AppName: "test", UserID: "u", SessionID: "s-n1"})
	if err != nil {
		t.Fatal(err)
	}
	sib := session.NewEvent(t.Context(), "inv")
	sib.Author = "sibling"
	sib.NodeInfo = &session.NodeInfo{Path: "root@1/sibling@1"}
	sib.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "s1", Name: "web_search", Args: map[string]any{"query": "SIBLING QUERY"}}}}}
	if err := sess.AppendEvent(t.Context(), resp.Session, sib); err != nil {
		t.Fatal(err)
	}
	stub := &scriptStub{texts: []string{""}, scores: []float64{0.95}}
	runGate(t, stub, &stopCtrl{}, sess, true) // every round stays empty: the node ends with no answer
	stub.mu.Lock()
	defer stub.mu.Unlock()
	sawWriter := false
	for _, r := range stub.requests {
		sawWriter = sawWriter || strings.Contains(r, "A response of 0 length")
		if strings.Contains(r, "SIBLING QUERY") {
			t.Fatalf("a worker/writer prompt carried a sibling's search:\n%s", r)
		}
	}
	if !sawWriter {
		t.Fatal("the tool-less writer never ran")
	}
}

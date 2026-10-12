package dag_test

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

const steerText = "STEER-NOW: answer with what you have"

// steerInterruptStub: the worker's first call starts a tool that blocks until its round is aborted; any
// later call answers, and the request that carries the steer is kept.
type steerInterruptStub struct {
	mu      sync.Mutex
	steered []*genai.Content
}

func (*steerInterruptStub) Name() string { return "steerInterruptStub" }

func (s *steerInterruptStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 3, "feedback": ""}), nil)
			return
		}
		last := req.Contents[len(req.Contents)-1]
		if !hasFunctionResponse(req.Contents) && !strings.Contains(last.Parts[len(last.Parts)-1].Text, steerText) {
			yield(atCall("block", map[string]any{}), nil)
			return
		}
		if last.Role == genai.RoleUser && strings.Contains(last.Parts[len(last.Parts)-1].Text, steerText) {
			s.mu.Lock()
			s.steered = req.Contents
			s.mu.Unlock()
		}
		yield(atText("STEERED-ANSWER"), nil)
	}
}

func blockingTool(t *testing.T, started chan<- struct{}) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[struct{}, map[string]any](
		functiontool.Config{Name: "block", Description: "blocks until its round is aborted"},
		func(ctx adkagent.Context, _ struct{}) (map[string]any, error) {
			started <- struct{}{}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Second):
				return nil, fmt.Errorf("the steer never aborted the round")
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// A steer queued while a native worker sits in a tool aborts the round at once and continues the worker's
// own session with the steer as its next user turn: the cut-off call is answered "interrupted" and the
// task is not sent again.
func TestQueueNodeMessage_InterruptsANativeRoundAndContinuesItsSession(t *testing.T) {
	testNativeWorkerSessions = session.InMemoryService()
	stub := &steerInterruptStub{}
	started := make(chan struct{}, 1)
	worker, err := llmagent.New(llmagent.Config{Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo",
		Tools: []tool.Tool{blockingTool(t, started)}})
	if err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p1", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"}}}
	ex := dag.NewExecutor(testNativeWorkerSessions, map[string]adkagent.Agent{"solo": testNativeWorker{Agent: worker}}, nil,
		vetting.NewJudgeFactory(passJudge, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)

	go func() {
		<-started
		if _, ok := ex.QueueNodeMessage("chat1", "n1", steerText); !ok {
			t.Error("node n1 was not live when its tool ran")
		}
	}()
	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "chat1", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(outputs["n1"], "STEERED-ANSWER") {
		t.Fatalf("n1 output = %q, want the steered answer", outputs["n1"])
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.steered == nil {
		t.Fatal("no worker request carried the steer as its last user turn")
	}
	var tasks, interrupted int
	for _, c := range stub.steered {
		for _, p := range c.Parts {
			if strings.Contains(p.Text, "Write the thing.") {
				tasks++
			}
			if fr := p.FunctionResponse; fr != nil && fr.Name == "block" && strings.Contains(fmt.Sprint(fr.Response), "interrupted") {
				interrupted++
			}
		}
	}
	if tasks != 1 || interrupted != 1 {
		t.Errorf("steered request has the task %d time(s) and %d interrupted block result(s), want 1 and 1", tasks, interrupted)
	}
	got, err := testNativeWorkerSessions.Get(context.Background(), &session.GetRequest{AppName: "solo",
		UserID: quackagent.WorkerSessionUser(quackagent.WorkerSessionID("chat1", "n1")), SessionID: quackagent.WorkerSessionID("chat1", "n1")})
	if err != nil || got.Session.Events().Len() == 0 {
		t.Fatalf("worker session: %v", err)
	}
}

func hasFunctionResponse(contents []*genai.Content) bool {
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				return true
			}
		}
	}
	return false
}

// A user pause aborts a native worker sitting in a tool at once and parks the node, keeping its worker session
// for the resume to continue.
func TestPauseNode_AbortsANativeRoundAndKeepsItsSession(t *testing.T) {
	testNativeWorkerSessions = session.InMemoryService()
	started := make(chan struct{}, 1)
	worker, err := llmagent.New(llmagent.Config{Name: "solo", Model: &steerInterruptStub{}, Description: "solo", Instruction: "ROLE:solo",
		Tools: []tool.Tool{blockingTool(t, started)}})
	if err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p1", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"}}}
	ex := dag.NewExecutor(testNativeWorkerSessions, map[string]adkagent.Agent{"solo": testNativeWorker{Agent: worker}}, nil,
		vetting.NewJudgeFactory(passJudge, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)

	go func() {
		<-started
		if !ex.PauseNode("chat1", "n1", dag.PauseUser) {
			t.Error("node n1 was not live when its tool ran")
		}
	}()
	var last string
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	start := time.Now()
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "chat1", content,
		func(ev stream.SSEEvent, _ error) bool { last = ev.Name; return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The blocking tool only gives up after 10s, so a cooperative pause would take that long.
	if last != stream.EventNodePaused || time.Since(start) > 5*time.Second {
		t.Fatalf("last event %q after %s, want node_paused with the round cut off", last, time.Since(start))
	}
	got, err := testNativeWorkerSessions.Get(context.Background(), &session.GetRequest{AppName: "solo",
		UserID: quackagent.WorkerSessionUser(quackagent.WorkerSessionID("chat1", "n1")), SessionID: quackagent.WorkerSessionID("chat1", "n1")})
	if err != nil || got.Session.Events().Len() == 0 {
		t.Fatalf("worker session was not kept: %v", err)
	}
}

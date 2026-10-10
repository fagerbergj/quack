package dag_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
	"iter"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

type probeStub struct {
	mu    sync.Mutex
	calls int
	saw   bool
}

func (*probeStub) Name() string { return "probeStub" }

func (s *probeStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		if n > 1 && strings.Contains(atAllText(req), "RUN-ONE-OUTPUT") {
			s.saw = true
		}
		s.mu.Unlock()
		if n == 1 {
			yield(atText("RUN-ONE-OUTPUT"), nil)
			return
		}
		yield(atText("RUN-TWO-OUTPUT"), nil)
	}
}

// Probes whether two separate RunPlanAsGraph calls for the same node identity see the same
// remote session history when nothing deletes it in between.
func TestNodeOverA2A_ReusedAcrossSeparateRunPlanAsGraphInvocations(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &probeStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo Answer.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	srv, err := quackagent.Serve(worker, sessions, nil, nil, quackagent.Compaction{}, "", nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node", "test-ctx")
	if err != nil {
		t.Fatalf("a2a client: %v", err)
	}

	run := func(chatID string) string {
		plan := dag.Plan{ID: "p-" + chatID, UserMessage: "x", Nodes: []dag.Node{
			{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"},
		}}
		ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"solo": client}, nil,
			vetting.NewJudgeFactory(passJudge, nil, nil),
			func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)
		outputs := map[string]string{}
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
		if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", chatID, content,
			func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
			t.Fatalf("run %s: %v", chatID, err)
		}
		return outputs["n1"]
	}

	if got := run("c1"); !strings.Contains(got, "RUN-ONE-OUTPUT") {
		t.Fatalf("run 1 output = %q", got)
	}
	if got := run("c1"); !strings.Contains(got, "RUN-TWO-OUTPUT") {
		t.Fatalf("run 2 output = %q", got)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.saw {
		t.Error("second RunPlanAsGraph invocation did not see the first's history - cross-turn reuse via a stable A2A contextID is broken")
	}
}

// testNativeWorker mirrors serve's nativeAgent.build: a fresh per-node A2A server, a client
// scoped to WorkerSessionID(chat, node), and a release that never deletes the session.
type testNativeWorker struct{ adkagent.Agent }

func (w testNativeWorker) ForNode(_ context.Context, nodeKey, _ string, _ func() string, _ artifact.Service, _, _, chatID, nodeID string, sink func(stream.SSEEvent)) (adkagent.Agent, model.LLM, []tool.Tool, func(round int, turnID, headSHA, triggerAnnotation string), func(paused bool), error) {
	srv, err := quackagent.Serve(w.Agent, testNativeWorkerSessions, nil, nil, quackagent.Compaction{}, nodeID, sink)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	client, err := srv.ClientForNode(nodeKey, quackagent.WorkerSessionID(chatID, nodeID))
	if err != nil {
		_ = srv.Close()
		return nil, nil, nil, nil, nil, err
	}
	return client, nil, nil, nil, func(bool) { _ = srv.Close() }, nil
}

// Package-level so the run's and a later retry's dispatch share one session store, as one
// Executor's e.sessions does in production.
var testNativeWorkerSessions session.Service

// A native worker session outlives completion for reuse, so RetryPlanInNode must reap its
// target node's session first: retry is "same task, fresh session".
func TestRetryPlanInNode_NativeNodeGetsFreshSession(t *testing.T) {
	testNativeWorkerSessions = session.InMemoryService()
	stub := &probeStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo Answer.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	nativeWorker := testNativeWorker{Agent: worker}

	plan := dag.Plan{ID: "p1", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"},
	}}
	ex := dag.NewExecutor(testNativeWorkerSessions, map[string]adkagent.Agent{"solo": nativeWorker}, nil,
		vetting.NewJudgeFactory(passJudge, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)

	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "chat1", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !strings.Contains(outputs["n1"], "RUN-ONE-OUTPUT") {
		t.Fatalf("first run output = %q", outputs["n1"])
	}

	var retryOut map[string]string
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			o, rerr := ex.RetryPlanInNode(ctx, plan, "chat1", "n1", map[string]string{})
			retryOut = o
			return "done", rerr
		}, workflow.NodeConfig{})
	top, err := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	if err != nil {
		t.Fatalf("retry workflow: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatalf("retry runner: %v", err)
	}
	for _, rerr := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "retry"}}}, adkagent.RunConfig{}) {
		if rerr != nil {
			t.Fatalf("retry run: %v", rerr)
		}
	}
	if !strings.Contains(retryOut["n1"], "RUN-TWO-OUTPUT") {
		t.Fatalf("retry output = %q", retryOut["n1"])
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.saw {
		t.Error("retry's request saw the failed/original run's prior output - retry must dispatch on a fresh session, not resume")
	}
}

// isolationStub is probeStub that also flags any sight of the sibling node's marker text.
type isolationStub struct {
	mu          sync.Mutex
	calls       int
	ownFirst    string
	siblingText string
	sawOwn      bool
	leaked      bool
}

func (*isolationStub) Name() string { return "isolationStub" }

func (s *isolationStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		all := atAllText(req)
		if strings.Contains(all, s.siblingText) {
			s.leaked = true
		}
		if n > 1 && strings.Contains(all, s.ownFirst) {
			s.sawOwn = true
		}
		s.mu.Unlock()
		if n == 1 {
			yield(atText(s.ownFirst), nil)
			return
		}
		yield(atText(s.ownFirst+"-ROUND2"), nil)
	}
}

// Two sibling nodes in one chat, both reused across two runs, each see only their own
// prior output.
func TestNodeOverA2A_SiblingNodesDoNotShareSessionHistory(t *testing.T) {
	sessions := session.InMemoryService()
	stubA := &isolationStub{ownFirst: "NODE-A-OUTPUT-ONE", siblingText: "NODE-B-OUTPUT-ONE"}
	stubB := &isolationStub{ownFirst: "NODE-B-OUTPUT-ONE", siblingText: "NODE-A-OUTPUT-ONE"}
	agentFor := func(name string, stub model.LLM) adkagent.Agent {
		ag, err := llmagent.New(llmagent.Config{Name: name, Model: stub, Description: name, Instruction: "ROLE:" + name + " Answer."})
		if err != nil {
			t.Fatalf("worker agent %s: %v", name, err)
		}
		return testNativeWorker{Agent: ag}
	}
	testNativeWorkerSessions = sessions
	synthAg, err := llmagent.New(llmagent.Config{Name: "synth", Model: &probeStub{}, Description: "synth", Instruction: "ROLE:synth Answer."})
	if err != nil {
		t.Fatalf("synth agent: %v", err)
	}
	agents := map[string]adkagent.Agent{
		"a":     agentFor("a", stubA),
		"b":     agentFor("b", stubB),
		"synth": testNativeWorker{Agent: synthAg},
	}
	// n1/n2 are true siblings; synth only gives buildPlanGraph its single terminal.
	plan := dag.Plan{ID: "p1", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "a", Task: "Write A.", Rubric: "detailed"},
		{ID: "n2", AgentName: "b", Task: "Write B.", Rubric: "detailed"},
		{ID: "synth", AgentName: "synth", Task: "Combine.", DependsOn: []string{"n1", "n2"}},
	}}
	ex := dag.NewExecutor(sessions, agents, nil, vetting.NewJudgeFactory(passJudge, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)

	run := func() map[string]string {
		outputs := map[string]string{}
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
		if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "chat1", content,
			func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		return outputs
	}
	if out := run(); !strings.Contains(out["n1"], "NODE-A-OUTPUT-ONE") || !strings.Contains(out["n2"], "NODE-B-OUTPUT-ONE") {
		t.Fatalf("first run outputs = %+v", out)
	}
	if out := run(); !strings.Contains(out["n1"], "ROUND2") || !strings.Contains(out["n2"], "ROUND2") {
		t.Fatalf("second run outputs = %+v, want both nodes to see their own reuse", out)
	}

	for name, s := range map[string]*isolationStub{"a": stubA, "b": stubB} {
		s.mu.Lock()
		leaked, sawOwn := s.leaked, s.sawOwn
		s.mu.Unlock()
		if leaked {
			t.Errorf("node %q saw its sibling's history - session isolation is broken", name)
		}
		if !sawOwn {
			t.Errorf("node %q never saw its OWN prior output on reuse - test isn't exercising resume", name)
		}
	}
}

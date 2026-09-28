package orchestrator

import (
	"context"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

func drain(seq func(func(stream.SSEEvent, error) bool)) {
	for range seq {
	}
}

// pinProbe installs a Gen 1 copy of o's boot roster that counts its OnDead calls.
func pinProbe(o *Orchestrator) *atomic.Int32 {
	boot := o.executor.RosterFor(context.Background())
	var deaths atomic.Int32
	o.executor.SetRoster(&dag.Roster{Gen: 1, Agents: boot.Agents, Models: boot.Models, CfgFor: boot.CfgFor,
		Infos: []dag.AgentInfo{{Name: "web-researcher"}}, OnDead: func() { deaths.Add(1) }})
	return &deaths
}

// Every entrypoint's pin must be released on success, error and early-stop paths,
// or a retired roster (and its MCP processes) never dies.
func TestOrchestratorPinsAreBalanced(t *testing.T) {
	o := newTestOrch(t, &orchStub{replies: []*model.LLMResponse{stubText("ANSWER")}})
	deaths := pinProbe(o)
	bg := context.Background()

	drain(o.Run(bg, "u", "chat", SourceApp, "hello", nil))
	for range o.Run(bg, "u", "chat", SourceApp, "stop early", nil) {
		break
	}
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	drain(o.Run(cancelled, "u", "chat2", SourceApp, "cancelled", nil))
	drain(o.RetryNode(bg, "u", "no-plan", nil, "n1", ""))
	drain(o.RunBoundPlan(bg, "u", "bound", SourceApp, dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "n", AgentName: "ghost", Task: "t"}}}))
	o.StartNode(bg, "u", "no-plan", "n1", "", func(stream.SSEEvent, error) bool { return true })
	if _, err := o.BuildBoundPlan(bg, []dag.RawNode{{ID: "n", Agent: "ghost", Task: "t"}}, "m", nil, nil); err == nil {
		t.Fatal("BuildBoundPlan accepted an unknown agent")
	}

	if deaths.Load() != 0 {
		t.Fatal("current roster died")
	}
	o.executor.SetRoster(&dag.Roster{Gen: 2})
	if deaths.Load() != 1 {
		t.Fatalf("retired roster OnDead = %d after all runs ended, want 1 (a pin leaked)", deaths.Load())
	}
}

// blockingWorker parks every worker call until release closes; judge rounds pass.
type blockingWorker struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*blockingWorker) Name() string { return "blockingWorker" }

func (b *blockingWorker) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, "submit_verdict") {
			yield(stubCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		b.once.Do(func() { close(b.entered) })
		<-b.release
		yield(stubText("DONE"), nil)
	}
}

func TestOrchestratorRunHoldsItsPin(t *testing.T) {
	w := &blockingWorker{entered: make(chan struct{}), release: make(chan struct{})}
	o := newTestOrch(t, w)
	deaths := pinProbe(o)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		drain(o.RunBoundPlan(context.Background(), "u", "bound", SourceApp,
			dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{{ID: "n", AgentName: "web-researcher", Task: "t"}}}))
	}()
	<-w.entered
	o.executor.SetRoster(&dag.Roster{Gen: 2})
	if deaths.Load() != 0 {
		t.Fatal("roster died while its run's worker was still executing")
	}
	close(w.release)
	<-finished
	if deaths.Load() != 1 {
		t.Fatalf("OnDead = %d after the run ended, want 1", deaths.Load())
	}
}

// swapStub swaps the executor's roster just before the orchestrator commits its plan.
type swapStub struct {
	orchStub
	once    sync.Once
	swap    func()
	swapped atomic.Bool
}

func (s *swapStub) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if stubHasTool(req, "create_plan") {
		if _, ok := planIDFromRequest(req); ok {
			s.once.Do(func() { s.swap(); s.swapped.Store(true) })
		}
	}
	return s.orchStub.GenerateContent(ctx, req, stream)
}

// freshModel is the swapped-in roster's worker: it answers FROM-NEW and passes judge rounds.
type freshModel struct{}

func (freshModel) Name() string { return "freshModel" }

func (freshModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, "submit_verdict") {
			yield(stubCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		yield(stubText("FROM-NEW"), nil)
	}
}

func TestOrchestratorExecuteRunsOnPinnedRoster(t *testing.T) {
	stub := &swapStub{orchStub: orchStub{replies: []*model.LLMResponse{planCall()}}}
	o := newTestOrch(t, stub)
	pinProbe(o)
	freshWorker, err := llmagent.New(llmagent.Config{Name: "web-researcher", Model: freshModel{}, Description: "r", Instruction: "ROLE:r"})
	if err != nil {
		t.Fatal(err)
	}
	cfgFor := o.executor.RosterFor(context.Background()).CfgFor
	stub.swap = func() {
		o.executor.SetRoster(&dag.Roster{Gen: 2, Agents: map[string]adkagent.Agent{"web-researcher": freshWorker},
			Models: map[string]model.LLM{"web-researcher": freshModel{}}, Infos: []dag.AgentInfo{{Name: "web-researcher"}}, CfgFor: cfgFor})
	}
	runTurn(t, o, "research the thing")
	if !stub.swapped.Load() {
		t.Fatal("the roster swap never ran")
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); !strings.Contains(answer, "RESEARCH-RESULT") {
		t.Fatalf("answer = %q, want the pinned roster's RESEARCH-RESULT", answer)
	}
}

// planProbe hires agent via create_plan and records the tool's response.
type planProbe struct {
	agent string
	mu    sync.Mutex
	resp  map[string]any
}

func (*planProbe) Name() string { return "planProbe" }

func (p *planProbe) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, c := range req.Contents {
			for _, part := range c.Parts {
				if part.FunctionResponse != nil && part.FunctionResponse.Name == "create_plan" {
					p.mu.Lock()
					p.resp = part.FunctionResponse.Response
					p.mu.Unlock()
					yield(stubText("done"), nil)
					return
				}
			}
		}
		yield(stubCall("create_plan", map[string]any{"assignments": []any{map[string]any{"agent": p.agent, "task": "t"}}}), nil)
	}
}

// A reload after a turn pinned its roster leaves create_plan on that roster: an
// agent only the old generation has is hired, one only the new generation has is not.
func TestCreatePlanUsesPinnedRosterAcrossSwap(t *testing.T) {
	for _, tc := range []struct {
		agent  string
		hireOK bool
	}{{"web-researcher", true}, {"fresh", false}} {
		t.Run(tc.agent, func(t *testing.T) {
			probe := &planProbe{agent: tc.agent}
			o := newTestOrch(t, probe)
			pinProbe(o)
			pinned, done := o.executor.Pin(context.Background())
			defer done()
			o.executor.SetRoster(&dag.Roster{Gen: 2, Infos: []dag.AgentInfo{{Name: "fresh"}}})
			dag.SetAgentRoster([]dag.AgentInfo{{Name: "fresh"}})
			drain(o.Run(pinned, "u", "chat", SourceApp, "plan it", nil))
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if _, hired := probe.resp["plan_id"]; hired != tc.hireOK {
				t.Fatalf("create_plan(%s) response = %v, want hired=%v", tc.agent, probe.resp, tc.hireOK)
			}
		})
	}
}

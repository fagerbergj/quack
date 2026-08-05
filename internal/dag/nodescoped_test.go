// Two DAG nodes running the SAME configured agent concurrently must never
// share the mutable model/tool objects SetLedgerCoords/ledger.StampCoords
// stamp coordinates onto - a shared object races the stamp and misattributes
// one node's ledger events to its sibling's coordinates. buildGateNodes'
// nodeScopedWorker branch (graph.go) is the fix: an agent implementing it
// gets a FRESH worker/model/tools built per node, never shared.
//
// This file drives that mechanism directly with a minimal nodeScopedWorker
// double whose ForNode records every model/tools pair it builds.
package dag_test

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// nsBarrierStub answers current_date, then a final answer. Every DRAFT-round
// call (before current_date's response comes back) rendezvous on a shared
// 2-party barrier before answering, forcing the two nodes' draft rounds to
// genuinely overlap in wall-clock time - the condition #609's shared-object
// bug needed to misattribute. Per-CALL, not per-instance (plain wg.Done/Wait,
// no sync.Once): the barrier must still pair up correctly when both nodes'
// calls land on the SAME shared stub instance (nodeScopedStub's share=true
// case), where a per-instance guard would only ever fire for the first
// caller and leave the second unpaired.
type nsBarrierStub struct {
	nodeKey string
	wg      *sync.WaitGroup
}

func (s *nsBarrierStub) Name() string { return "nsBarrierStub" }

func (s *nsBarrierStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if lcHasFuncResponse(req, "current_date") {
			yield(lcText("done: "+s.nodeKey), nil)
			return
		}
		s.wg.Done()
		s.wg.Wait()
		yield(lcCall("current_date", map[string]any{}), nil)
	}
}

// nsSynthStub terminates the plan (ADK allows one terminal node) - a plain
// fan-in over n1/n2, not itself under test.
type nsSynthStub struct{}

func (nsSynthStub) Name() string { return "nsSynthStub" }
func (nsSynthStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(lcText("SUMMARY"), nil)
	}
}

// nsJudge always passes on the first verdict - the only thing under test is
// ledger attribution, not gate convergence.
type nsJudge struct{}

func (nsJudge) Name() string { return "nsJudge" }
func (nsJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(lcCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
	}
}

// nodeScopedStub is a minimal nodeScopedWorker double for the SAME
// configured agent used by concurrent plan nodes. share=true simulates the
// pre-fix shared-object bug: model and tools built ONCE and reused across
// every ForNode call, even though each node still gets a distinct client
// identity. Kept here only to prove this test is sensitive to the
// regression it pins.
type nodeScopedStub struct {
	adkagent.Agent // a throwaway prototype (never Run - ForNode always wins)
	share          bool
	wg             *sync.WaitGroup

	mu      sync.Mutex
	calls   int
	built   []string // "<nodeKey>:<model pointer>" per ForNode call, in order
	cachedM model.LLM
	cachedT []tool.Tool
}

func (s *nodeScopedStub) ForNode(nodeKey string) (adkagent.Agent, model.LLM, []tool.Tool, func(), error) {
	s.mu.Lock()
	s.calls++
	m, builtins := s.cachedM, s.cachedT
	s.mu.Unlock()

	if m == nil {
		stub := &nsBarrierStub{nodeKey: nodeKey, wg: s.wg}
		m = inference.TracedModelForTesting(stub, "nodeScopedStub")
		var err error
		if builtins, err = tools.Build([]string{"current_date"}, tools.Deps{}); err != nil {
			return nil, nil, nil, nil, err
		}
		if s.share {
			s.mu.Lock()
			s.cachedM, s.cachedT = m, builtins
			s.mu.Unlock()
		}
	}
	// A fresh CLIENT/wrapper identity per node either way - even the
	// pre-#609 shape gave every node its own client (the older
	// nodeClient.ForNode); only the model/tools underneath it were shared.
	worker, err := llmagent.New(llmagent.Config{
		Name: "w", Model: m, Description: "w",
		Instruction: "ROLE:w Call current_date, then answer.",
		Tools:       builtins,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}

	s.mu.Lock()
	s.built = append(s.built, fmt.Sprintf("%s:%p", nodeKey, m))
	s.mu.Unlock()
	return worker, m, builtins, func() {}, nil
}

// runTwoConcurrentNodes drives a 2-node, no-dependency plan (both nodes named
// "w", the SAME configured agent) through the real production entry point
// (dag.Executor.RunPlanAsGraph) with the given nodeScopedStub, and returns
// the quack.node attribute of each captured draft-round "chat" ledger event,
// in emission order. Correct attribution is exactly {"n1", "n2"} as a set
// (order depends on which of the two concurrent nodes' draft round the
// runner happens to schedule/emit first).
func runTwoConcurrentNodes(t *testing.T, stub *nodeScopedStub) []string {
	t.Helper()
	capExp := &ledgerCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	synth, err := llmagent.New(llmagent.Config{
		Name: "synth", Model: nsSynthStub{}, Description: "synth", Instruction: "ROLE:synth Summarize.",
	})
	if err != nil {
		t.Fatalf("synth agent: %v", err)
	}

	ex := dag.NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"w": stub, "synth": synth}, nil, nil,
		vetting.NewJudgeFactory(nsJudge{}, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)

	const chatID = "nodescoped-chat"
	plan := dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{
		{ID: "n1", AgentName: "w", Task: "answer for n1"},
		{ID: "n2", AgentName: "w", Task: "answer for n2"},
		{ID: "synth", AgentName: "synth", Task: "Summarize both.", DependsOn: []string{"n1", "n2"}},
	}}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", chatID, content,
		func(stream.SSEEvent, error) bool { return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	var nodes []string
	for _, r := range capExp.records {
		attrs := ledgerAttrsOf(r)
		// Only the "w"-agent (n1/n2) draft rounds are under test - the synth
		// fan-in node's own chat event (agent "synth") is required for the
		// graph to have one terminal node but isn't part of the assertion.
		if attrs["gen_ai.operation.name"] != "chat" || attrs["gen_ai.agent.name"] != "w" {
			continue
		}
		nodes = append(nodes, attrs["quack.node"])
	}
	return nodes
}

// TestNodeScopedWorker_PerNodeConstruction_IsolatesLedgerAttribution: two
// concurrent nodes sharing ONE configured agent each get a FRESH model/tools
// pair (nodeScopedStub.share=false, ForNode called twice, two DISTINCT
// objects), forced to genuinely overlap via nsBarrierStub's barrier. Because
// nothing is shared, each node's ledger events must carry ITS OWN node id -
// this can never misattribute, structurally, regardless of timing.
func TestNodeScopedWorker_PerNodeConstruction_IsolatesLedgerAttribution(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)
	stub := &nodeScopedStub{wg: &wg}

	nodes := runTwoConcurrentNodes(t, stub)

	if stub.calls != 2 {
		t.Fatalf("ForNode called %d times, want 2 (one per node)", stub.calls)
	}
	if len(stub.built) != 2 || stub.built[0] == stub.built[1] {
		t.Fatalf("built pairs = %v, want two DISTINCT model instances (one per node)", stub.built)
	}

	// Each node makes TWO model calls (current_date, then the final answer) -
	// correct attribution is exactly two "n1" and two "n2" events, never a
	// count that leaks one node's round onto the other.
	if c := counts(nodes); c["n1"] != 2 || c["n2"] != 2 || len(nodes) != 4 {
		t.Errorf("draft/final chat events carried quack.node = %v, want exactly two n1 and two n2", nodes)
	}
}

// TestNodeScopedWorker_SharedObjectMisattributes proves the test above is
// sensitive to the actual #609 bug: with share=true (one model/tools pair
// reused across both ForNode calls - the pre-fix shape, where buildAgents
// built a native agent's model/tools ONCE per agent name), the SAME two
// concurrent, barrier-forced-overlapping nodes misattribute at least one
// ledger event to the wrong node id. This is the "make it fail first" half
// of #609's regression coverage, kept as a permanent assertion (not a
// disabled/skipped test) so a future change that reintroduces sharing here
// is caught by CI, not rediscovered live.
func TestNodeScopedWorker_SharedObjectMisattributes(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)
	stub := &nodeScopedStub{share: true, wg: &wg}

	nodes := runTwoConcurrentNodes(t, stub)

	if stub.calls != 2 {
		t.Fatalf("ForNode called %d times, want 2 (one per node)", stub.calls)
	}

	if c := counts(nodes); c["n1"] == 2 && c["n2"] == 2 && len(nodes) == 4 {
		t.Fatalf("draft/final chat events carried quack.node = %v - both nodes attributed correctly despite "+
			"sharing one model/tools pair under forced concurrent overlap; either the shared stub isn't "+
			"exercising the race, or SetLedgerCoords stopped being a shared mutable field", nodes)
	}
}

func counts(vs []string) map[string]int {
	out := make(map[string]int, len(vs))
	for _, v := range vs {
		out[v]++
	}
	return out
}

// Concurrent nodes of the SAME agent must never share the model/tool objects ledger coords
// are stamped onto; nodeScopedWorker gives each node fresh ones.
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
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// nsBarrierStub calls current_date then answers; each draft-round call meets a 2-party
// barrier first (per call, so it pairs even on a shared instance) to force overlap.
type nsBarrierStub struct {
	nodeKey string
	wg      *sync.WaitGroup
}

func (s *nsBarrierStub) Name() string { return "nsBarrierStub" }

func (s *nsBarrierStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasFuncResponse(req, "current_date") {
			yield(atText("done: "+s.nodeKey), nil)
			return
		}
		s.wg.Done()
		s.wg.Wait()
		yield(atCall("current_date", map[string]any{}), nil)
	}
}

// nodeScopedStub is a nodeScopedWorker double; share=true reuses one model/tools pair
// across ForNode calls to prove the test catches the shared-object bug.
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

func (s *nodeScopedStub) ForNode(_ context.Context, nodeKey, _ string, _ func() string, _ artifact.Service, _, _, _, _ string, _ func(stream.SSEEvent)) (adkagent.Agent, model.LLM, []tool.Tool, ledger.CoordSetter, func(int, string, string, string), func(context.Context) artifactsrc.Artifact, func(bool), error) {
	s.mu.Lock()
	s.calls++
	m, builtins := s.cachedM, s.cachedT
	s.mu.Unlock()

	if m == nil {
		stub := &nsBarrierStub{nodeKey: nodeKey, wg: s.wg}
		m = inference.TracedModelForTesting(stub, "nodeScopedStub")
		var err error
		if builtins, err = tools.Build([]string{"current_date"}, tools.Deps{}); err != nil {
			return nil, nil, nil, nil, nil, nil, nil, err
		}
		if s.share {
			s.mu.Lock()
			s.cachedM, s.cachedT = m, builtins
			s.mu.Unlock()
		}
	}
	// A fresh client identity per node either way; only share=true shares model/tools.
	worker, err := llmagent.New(llmagent.Config{
		Name: "w", Model: m, Description: "w",
		Instruction: "ROLE:w Call current_date, then answer.",
		Tools:       builtins,
	})
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}

	s.mu.Lock()
	s.built = append(s.built, fmt.Sprintf("%s:%p", nodeKey, m))
	s.mu.Unlock()
	return worker, m, builtins, nil, nil, nil, func(bool) {}, nil
}

// runTwoConcurrentNodes runs two concurrent nodes of agent "w" and returns each draft-round
// chat event's quack.node in emission order; correct is {"n1","n2"} as a set.
func runTwoConcurrentNodes(t *testing.T, stub *nodeScopedStub) []string {
	t.Helper()
	capExp := &ledgerCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	synth, err := llmagent.New(llmagent.Config{
		Name: "synth", Model: textLLM("SUMMARY"), Description: "synth", Instruction: "ROLE:synth Summarize.",
	})
	if err != nil {
		t.Fatalf("synth agent: %v", err)
	}

	ex := dag.NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"w": stub, "synth": synth}, nil,
		vetting.NewJudgeFactory(passJudge, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)

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
		// Only the "w" draft rounds are under test; synth exists for a single terminal.
		if attrs["gen_ai.operation.name"] != "chat" || attrs["gen_ai.agent.name"] != "w" {
			continue
		}
		nodes = append(nodes, attrs["quack.node"])
	}
	return nodes
}

// With fresh model/tools per node, each node's ledger events carry its own id, whatever
// the timing.
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

	// Each node makes two calls (current_date, final answer): exactly two per node.
	if c := counts(nodes); c["n1"] != 2 || c["n2"] != 2 || len(nodes) != 4 {
		t.Errorf("draft/final chat events carried quack.node = %v, want exactly two n1 and two n2", nodes)
	}
}

// With share=true the same overlapping nodes misattribute at least one event, proving the
// test above is sensitive to the bug; kept live so CI catches a reintroduction.
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

package replaytest

// buildFixtureBundle and its scripted model/tool are test-only support: they
// run a real 2-node gated-refine session through the ACTUAL recording seams
// (tracedModel + emitWrap + the ledger exporter), then assemble the result
// into a bundle.zip - the fixture every replaytest_test.go case replays.
// There are no production recordings yet (.quack/replay-log.md); this is
// how the engine's own tests get a realistic one.

import (
	"bytes"
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
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

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

const (
	fixtureChatID      = "fixture-chat"
	fixtureWorkerModel = "worker-model"
	fixtureJudgeModel  = "judge-model"
	fixtureToolName    = "web_search"
	fixtureNodeA       = "node-a"
	fixtureNodeB       = "node-b"
	fixturePromptA     = "What is the capital of France?"
	fixturePromptB     = "Name a river in Germany."
)

// fixtureCfg is the vetting.Config every fixture node (and every replaytest
// case replaying one) uses - one round of judge failure then a pass, same
// shape as vetting's own TestGatedWorkerNode_RefineLoopConverges.
func fixtureCfg(nodeID string) vetting.Config {
	return vetting.Config{
		ChatID: fixtureChatID, NodeID: nodeID, Agent: "worker",
		JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10",
	}
}

// scriptedModel is a deterministic model.LLM standing in for BOTH the
// worker and the judge (routed by request shape, same trick vetting's own
// tests use): draft calls a tool once then answers; the judge fails the
// draft, passes the revision.
type scriptedModel struct{ name string }

func (m *scriptedModel) Name() string { return m.name }

func (m *scriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		text := fixtureAllText(req)
		switch {
		case fixtureHasTool(req, "submit_verdict"):
			score := 0.4
			if strings.Contains(text, "revised") {
				score = 0.9
			}
			yield(fixtureCall("submit_verdict", map[string]any{"score": score, "feedback": "tighten the claims"}), nil)
		case fixtureHasFuncResponse(req, fixtureToolName):
			yield(fixtureText("This is the initial draft answer."), nil)
		case strings.Contains(text, "Verdict:"):
			yield(fixtureText("This is the revised answer with the reviewer's fixes applied."), nil)
		default:
			yield(fixtureCall(fixtureToolName, map[string]any{"query": text}), nil)
		}
	}
}

func fixtureHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func fixtureHasFuncResponse(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return true
			}
		}
	}
	return false
}

func fixtureAllText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func fixtureText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func fixtureCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

// searchArgs/searchResult give the fixture tool a realistic typed shape -
// functiontool.New infers its JSON schema from these, same as every real
// builtin (internal/tools/websearch.go).
type searchArgs struct {
	Query string `json:"query"`
}
type searchResult struct {
	Results []string `json:"results"`
}

// newFixtureTool builds the ONE fake tool the fixture's worker uses,
// wrapped through the real execute_tool emission seam
// (tools.EmitWrapForTesting) so its recorded ledger entry matches a builtin
// tool's. coords is stamped directly (Deps.LedgerCoords' testing twin)
// rather than left to ctx: workflow.RunNode's dynamic-child scheduling
// doesn't propagate a context.WithValue stamp down to a tool call, and a
// tool built once per node before any round exists has no per-round hook -
// so this only ever carries node/agent (Round empty; see emitTool's doc).
func newFixtureTool(t *testing.T, coords ledger.Coords) tool.Tool {
	t.Helper()
	ft, err := functiontool.New[searchArgs, searchResult](
		functiontool.Config{Name: fixtureToolName, Description: "fixture search tool"},
		func(_ adkagent.Context, a searchArgs) (searchResult, error) {
			return searchResult{Results: []string{"quack replay result for: " + a.Query}}, nil
		},
	)
	if err != nil {
		t.Fatalf("fixture: build tool: %v", err)
	}
	wrapped, err := tools.EmitWrapForTesting(ft, coords)
	if err != nil {
		t.Fatalf("fixture: wrap tool: %v", err)
	}
	return wrapped
}

// runFixtureNode drives ONE gated-refine node to completion through the
// real emission seams - worker + judge each a scriptedModel wrapped by
// inference.TracedModelForTesting (the seam that emits the "chat" ledger
// event, coordinated per round by vetting.RunGatedRefine's
// SetLedgerCoords call), the tool wrapped by tools.EmitWrapForTesting
// (stamped once, per node, above).
func runFixtureNode(t *testing.T, nodeID, prompt string) {
	t.Helper()
	workerModel := inference.TracedModelForTesting(&scriptedModel{name: fixtureWorkerModel}, fixtureWorkerModel)
	judgeModel := inference.TracedModelForTesting(&scriptedModel{name: fixtureJudgeModel}, fixtureJudgeModel)
	toolCoords := ledger.Coords{ChatID: fixtureChatID, Node: nodeID, Agent: "worker"}

	worker, err := llmagent.New(llmagent.Config{
		Name: "worker", Model: workerModel, Description: "fixture worker",
		Instruction: "Answer the question. Use web_search first.",
		Tools:       []tool.Tool{newFixtureTool(t, toolCoords)},
	})
	if err != nil {
		t.Fatalf("fixture: build worker agent: %v", err)
	}
	workerNode, err := vetting.NewWorkerNode(worker)
	if err != nil {
		t.Fatalf("fixture: build worker node: %v", err)
	}
	judgeFactory := vetting.NewJudgeFactory(judgeModel, nil, nil)
	cfg := fixtureCfg(nodeID)

	fn := func(ctx adkagent.Context, task string, emit func(*session.Event) error) (string, error) {
		if strings.TrimSpace(task) == "" {
			task = prompt
		}
		answer, _, err := vetting.RunGatedRefine(ctx, nodeID, workerNode, workerModel, judgeFactory, cfg, task, nil, nil, emit)
		return answer, err
	}
	node := workflow.NewDynamicNode[string, string](nodeID, fn, workflow.NodeConfig{})

	root, err := workflowagent.New(workflowagent.Config{
		Name: "fixture-root-" + nodeID, SubAgents: []adkagent.Agent{worker},
		Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("fixture: build root workflow: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "fixture", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("fixture: build runner: %v", err)
	}

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}
	for _, err := range r.Run(t.Context(), "u", "s-"+nodeID, task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("fixture: run %s: %v", nodeID, err)
		}
	}
}

// buildFixtureBundle records a 2-node gated-refine session and returns the
// path to its assembled bundle.zip.
func buildFixtureBundle(t *testing.T) string {
	t.Helper()
	store := ledger.NewMemStore()

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(ledger.NewRedactingProcessor()),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store))),
	)
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	runFixtureNode(t, fixtureNodeA, fixturePromptA)
	runFixtureNode(t, fixtureNodeB, fixturePromptB)

	return assembleFixtureBundle(t, store)
}

// assembleFixtureBundle reads back fixtureChatID's recorded entries and
// zips them - the same path ledger.AssembleBundle's own round-trip test
// exercises (internal/ledger/bundle_test.go).
func assembleFixtureBundle(t *testing.T, store ledger.LedgerStore) string {
	t.Helper()
	var buf bytes.Buffer
	if err := ledger.AssembleBundle(context.Background(), store, fixtureChatID, "test", &buf); err != nil {
		t.Fatalf("fixture: assemble bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "bundle.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("fixture: write bundle: %v", err)
	}
	return path
}

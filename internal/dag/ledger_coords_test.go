// Package dag_test: needs both the real dag.Executor/gate machinery and
// internal/tools.Build; internal/tools already imports internal/dag, so a
// same-package test can't import tools back without a cycle - only an
// external test package can.
//
// Pins a real regression: workflow.RunNode's dynamic-child scheduling does
// NOT propagate a context.WithValue stamp down to the model/tool call
// underneath it, so worker-round ledger events lost node/agent attribution
// until tracedModel/emitTool.SetLedgerCoords plus newGatedNode's
// ledger.StampCoords carried coordinates through explicitly.
package dag_test

import (
	"context"
	"iter"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
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

// ledgerCaptureExporter records every emitted log record - the simplest
// possible sdklog.Exporter for inspecting attrs directly (mirrors
// internal/inference/emit_test.go's captureExporter).
type ledgerCaptureExporter struct{ records []sdklog.Record }

func (c *ledgerCaptureExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *ledgerCaptureExporter) Shutdown(context.Context) error   { return nil }
func (c *ledgerCaptureExporter) ForceFlush(context.Context) error { return nil }

// ledgerAttrsOf collects only the STRING-valued attributes (every field
// this test asserts on is one) - AsString() on a non-string Kind (e.g. the
// Slice-valued gen_ai.tool.definitions) logs a spurious internal warning.
func ledgerAttrsOf(r sdklog.Record) map[string]string {
	out := map[string]string{}
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Value.Kind() == otellog.KindString {
			out[string(kv.Key)] = kv.Value.AsString()
		}
		return true
	})
	return out
}

// lcScopedAgent is a minimal nodeScopedWorker double: ForNode returns the
// same worker/model/tools it was built with, so buildGateNodes' scoped path
// (the one production always takes) is what stamps ledger coords here too.
type lcScopedAgent struct {
	adkagent.Agent
	model model.LLM
	tools []tool.Tool
}

func (a lcScopedAgent) ForNode(string) (adkagent.Agent, model.LLM, []tool.Tool, func(), error) {
	return a.Agent, a.model, a.tools, func() {}, nil
}

// ledgerCoordsStub calls current_date once, answers, and passes the judge
// immediately (JudgeRounds:1, first verdict scores 0.9) - the shortest
// path that still produces one chat AND one execute_tool event. Local
// duplicate of dag's own gCall/gText/gHasTool stub helpers - unexported
// there (see ask_advisor_test.go's identical note).
type ledgerCoordsStub struct{}

func (ledgerCoordsStub) Name() string { return "ledgerCoordsStub" }

func (ledgerCoordsStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case lcHasTool(req, "submit_verdict"):
			yield(lcCall("submit_verdict", map[string]any{"score": 0.9, "feedback": "fine"}), nil)
		case lcHasFuncResponse(req, "current_date"):
			yield(lcText("today's date, as reported by the tool, is noted"), nil)
		default:
			yield(lcCall("current_date", map[string]any{}), nil)
		}
	}
}

func lcHasTool(req *model.LLMRequest, name string) bool {
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

func lcHasFuncResponse(req *model.LLMRequest, name string) bool {
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

func lcText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func lcCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

// TestRunPlanAsGraph_LedgerCoordsReachModelAndTool drives ONE gated node
// through the real production entry point (dag.Executor.RunPlanAsGraph, the
// same call serve.go makes) and asserts both the "chat" and the
// "execute_tool" ledger events it emits carry the run's coordinates.
func TestRunPlanAsGraph_LedgerCoordsReachModelAndTool(t *testing.T) {
	capExp := &ledgerCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	// The SAME wrapping seams production uses: inference.NewModel always
	// wraps in tracedModel; tools.Build always wraps a builtin in emitTool.
	stub := ledgerCoordsStub{}
	workerModel := inference.TracedModelForTesting(stub, "ledger-coords-model")
	builtins, err := tools.Build([]string{"current_date"}, tools.Deps{})
	if err != nil {
		t.Fatalf("tools.Build: %v", err)
	}

	worker, err := llmagent.New(llmagent.Config{
		Name: "w", Model: workerModel, Description: "w",
		Instruction: "ROLE:w Answer, calling current_date first.", Tools: builtins,
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	// Production always dispatches through a nodeScopedWorker (nativeAgent's
	// ForNode) so ledger.StampCoords reaches the tools actually invoked;
	// mirror that here rather than relying on the plain agent.
	scoped := lcScopedAgent{Agent: worker, model: workerModel, tools: builtins}

	ex := dag.NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"w": scoped},
		map[string]model.LLM{"w": workerModel},
		vetting.NewJudgeFactory(workerModel, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} },
		nil)

	const chatID = "ledger-coords-chat"
	plan := dag.Plan{ID: "t", UserMessage: "what's today's date?", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "answer"}}}

	ctx := stream.WithYield(context.Background(), func(stream.SSEEvent) {})
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}
	if _, err := ex.RunPlanAsGraph(ctx, plan, "quack", "u", chatID, content, func(stream.SSEEvent, error) bool { return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	var chatAttrs, toolAttrs map[string]string
	for _, r := range capExp.records {
		attrs := ledgerAttrsOf(r)
		switch attrs["gen_ai.operation.name"] {
		case "chat":
			if chatAttrs == nil {
				chatAttrs = attrs
			}
		case "execute_tool":
			if toolAttrs == nil {
				toolAttrs = attrs
			}
		}
	}

	if chatAttrs == nil {
		t.Fatal("no chat ledger event recorded")
	}
	if got := chatAttrs["gen_ai.conversation.id"]; got != chatID {
		t.Errorf("chat gen_ai.conversation.id = %q, want %q", got, chatID)
	}
	if got := chatAttrs["quack.node"]; got != "n1" {
		t.Errorf("chat quack.node = %q, want %q", got, "n1")
	}
	if got := chatAttrs["gen_ai.agent.name"]; got != "w" {
		t.Errorf("chat gen_ai.agent.name = %q, want %q", got, "w")
	}

	if toolAttrs == nil {
		t.Fatal("no execute_tool ledger event recorded")
	}
	if got := toolAttrs["gen_ai.conversation.id"]; got != chatID {
		t.Errorf("execute_tool gen_ai.conversation.id = %q, want %q", got, chatID)
	}
	if got := toolAttrs["quack.node"]; got != "n1" {
		t.Errorf("execute_tool quack.node = %q, want %q", got, "n1")
	}
	if got := toolAttrs["gen_ai.agent.name"]; got != "w" {
		t.Errorf("execute_tool gen_ai.agent.name = %q, want %q", got, "w")
	}
}

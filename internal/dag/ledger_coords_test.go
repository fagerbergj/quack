// External test package: internal/tools imports internal/dag, so only dag_test can use
// tools.Build. RunNode drops ctx values, so coords must be stamped explicitly.
package dag_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
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

// ledgerCaptureExporter records every emitted log record.
type ledgerCaptureExporter struct{ records []sdklog.Record }

func (c *ledgerCaptureExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *ledgerCaptureExporter) Shutdown(context.Context) error   { return nil }
func (c *ledgerCaptureExporter) ForceFlush(context.Context) error { return nil }

// ledgerAttrsOf collects only string attributes: AsString() on another Kind logs a
// spurious internal warning.
func ledgerAttrsOf(r sdklog.Record) map[string]string {
	out := map[string]string{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Value.Type() == attribute.STRING {
			out[string(kv.Key)] = kv.Value.AsString()
		}
		return true
	})
	return out
}

// lcScopedAgent's ForNode returns its fixed worker/model/tools, so the scoped path
// production always takes is what stamps ledger coords.
type lcScopedAgent struct {
	adkagent.Agent
	model model.LLM
	tools []tool.Tool
}

func (a lcScopedAgent) ForNode(context.Context, string, string, func() string, artifact.Service, string, string, string, string, func(stream.SSEEvent)) (adkagent.Agent, model.LLM, []tool.Tool, func(int, string, string, string), func(context.Context) artifactsrc.Artifact, func(bool), error) {
	return a.Agent, a.model, a.tools, nil, nil, func(bool) {}, nil
}

// Through the production entry point, both "chat" and "execute_tool" ledger events carry
// the run's coordinates.
func TestRunPlanAsGraph_LedgerCoordsReachModelAndTool(t *testing.T) {
	capExp := &ledgerCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	// The SAME wrapping seams production uses: inference.NewModel always
	// wraps in tracedModel; tools.Build always wraps a builtin in emitTool.
	stub := dateToolLLM("today's date, as reported by the tool, is noted")
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

	// Production dispatches through a nodeScopedWorker, so StampCoords reaches the invoked tools.
	scoped := lcScopedAgent{Agent: worker, model: workerModel, tools: builtins}

	ex := dag.NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"w": scoped},
		map[string]model.LLM{"w": workerModel},
		vetting.NewJudgeFactory(workerModel, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} },
		nil)

	const chatID = "ledger-coords-chat"
	plan := dag.Plan{ID: "t", UserMessage: "what's today's date?", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "answer"}}}

	// Seed partial coords as orchestrator.go does; a bare ctx let a broken
	// coords-precedence fix pass.
	ctx := stream.WithYield(
		ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID, User: "u", Source: "ui"}),
		func(stream.SSEEvent) {})
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

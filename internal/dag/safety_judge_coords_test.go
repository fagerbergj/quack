// Measures ledger.CoordsFromContext when runSafetyJudge fires inside a guarded tool call
// on a real worker node (external package: see ledger_coords_test.go).
package dag_test

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// Wraps current_date as a judge-tier guarded tool and records the coords the judge func sees.
func TestSafetyJudgeCoords_MeasuredAtCallTime(t *testing.T) {
	stub := dateToolLLM("done")
	workerModel := inference.TracedModelForTesting(stub, "sj-coords-model")

	var captured ledger.Coords
	var calls int
	measuringJudge := tools.SafetyJudge(func(ctx context.Context, _, _, _ string, _ map[string]any, _ string) (bool, string, error) {
		calls++
		captured = ledger.CoordsFromContext(ctx)
		return true, "on task", nil
	})

	builtins, err := tools.Build([]string{"current_date"}, tools.Deps{
		Guards:      map[string]string{"current_date": "judge"},
		SafetyJudge: measuringJudge,
	})
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

	scoped := lcScopedAgent{Agent: worker, model: workerModel, tools: builtins}

	ex := dag.NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"w": scoped},
		map[string]model.LLM{"w": workerModel},
		vetting.NewJudgeFactory(workerModel, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} },
		nil)

	const chatID = "sj-coords-chat"
	plan := dag.Plan{ID: "t", UserMessage: "what's today's date?", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "answer"}}}

	ctx := stream.WithYield(
		ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID, User: "u", Source: "ui"}),
		func(stream.SSEEvent) {})
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}
	if _, err := ex.RunPlanAsGraph(ctx, plan, "quack", "u", chatID, content, func(stream.SSEEvent, error) bool { return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if calls == 0 {
		t.Fatal("safety judge never fired - guard wiring is broken, nothing was measured")
	}
	t.Logf("MEASURED ledger.CoordsFromContext(ctx) inside runSafetyJudge: %+v (calls=%d)", captured, calls)

	// The safety judge fires on this node's worker, so its ledger events carry this node's attribution.
	if captured.ChatID != chatID {
		t.Errorf("ChatID = %q, want %q", captured.ChatID, chatID)
	}
	if captured.Node != "n1" {
		t.Errorf("Node = %q, want %q", captured.Node, "n1")
	}
	if captured.Agent != "w" {
		t.Errorf("Agent = %q, want %q", captured.Agent, "w")
	}
}

var _ tool.Tool // keep tool import if unused elsewhere

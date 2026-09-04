// This file drives internal/orchestrator's REAL entry point (Orchestrator.Run)
// through the REAL ledger.Exporter/MemStore, then feeds the resulting bundle
// back into replay.Load - proving UserTurns() works against a bundle shaped
// the way PRODUCTION actually writes one, not a hand-built fixture that
// happens to only ever go through one exporter (see #617: before the fix,
// root/orchestrator-level events carried no ledger.Coords at all and fell
// into the shared "unscoped" bucket, so no chat's own bundle ever had a root
// stream for UserTurns() to read).
package replay_test

import (
	"context"
	"iter"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/vetting"
)

// answerStub is a model.LLM that always answers with a fixed text on the
// first call - enough to drive a normal (no plan/tool) orchestrator turn.
type answerStub struct{ text string }

func (answerStub) Name() string { return "answer-stub" }

func (a answerStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: a.text}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// newOrchForTest builds an Orchestrator with its model wrapped in
// tracedModel - exactly how production's inference.NewModel wraps every
// model - so its "chat" ledger events flow through the real emitChatEvent
// seam this test needs.
func newOrchForTest(t *testing.T, m model.LLM) *orchestrator.Orchestrator {
	t.Helper()
	traced := inference.TracedModelForTesting(m, "orch-model")
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: m, Description: "researcher", Instruction: "ROLE:researcher",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"web-researcher": worker},
		map[string]model.LLM{"web-researcher": m},
		vetting.NewJudgeFactory(m, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, nil)
	return orchestrator.New(sessions, traced, "You are the orchestrator.", planner, ex, nil, nil, nil)
}

// TestUserTurns_FromProductionShapedBundle is the #617 regression that
// matters most: UserTurns() must recover the user's turn from a bundle built
// the way a live run actually records one - root events stamped with ChatID
// only, filed by the real exporter under this chat's own id.
func TestUserTurns_FromProductionShapedBundle(t *testing.T) {
	store := ledger.NewMemStore()

	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store))))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	const chatID = "prod-shaped-chat"
	const userMsg = "are ducks birds?"
	o := newOrchForTest(t, answerStub{text: "Ducks are birds."})
	for _, runErr := range o.Run(context.Background(), "u", chatID, orchestrator.SourceApp, userMsg, nil) {
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
	}

	sess, err := replay.FromStore(context.Background(), store, chatID)
	if err != nil {
		t.Fatalf("replay.FromStore: %v (root events fell into no chat stream?)", err)
	}

	turns := sess.UserTurns()
	if len(turns) == 0 {
		t.Fatal("UserTurns() returned no turns - the chat's own bundle has no root stream (StreamKey{}) for eval to read")
	}
	if turns[0] != userMsg {
		t.Errorf("UserTurns()[0] = %q, want %q", turns[0], userMsg)
	}
}

// This file drives Orchestrator.Run through the REAL ledger.Exporter/MemStore and replays the bundle:
// UserTurns() must work against a production-shaped bundle (#617: before the fix, root events had no
// ledger.Coords and fell into the shared "unscoped" bucket, so no chat's bundle ever had a root stream).
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
	"github.com/fagerbergj/quack/internal/ledgertest"
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

// newOrchForTest wraps its model in tracedModel - exactly how production's
// inference.NewModel wraps every model - so "chat" ledger events flow through
// the real emitChatEvent seam.
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

// TestUserTurns_FromProductionShapedBundle (#617 regression): UserTurns()
// recovers the user's turn from a bundle recorded the way a live run records
// it - root events stamped with ChatID only, filed under the chat's own id.
func TestUserTurns_FromProductionShapedBundle(t *testing.T) {
	store := ledgertest.NewMemStore()

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

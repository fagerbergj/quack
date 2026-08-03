// This file drives internal/orchestrator's REAL entry point (Orchestrator.Run)
// through the REAL ledger.Exporter/FSStore, then feeds the resulting bundle
// back into replay.Load - proving UserTurns() works against a bundle shaped
// the way PRODUCTION actually writes one, not a hand-built fixture that
// happens to only ever go through one exporter (see #617: before the fix,
// root/orchestrator-level events carried no ledger.Coords at all and fell
// into the shared "unscoped" bucket, so no chat's own bundle ever had a root
// stream for UserTurns() to read).
package replay_test

import (
	"context"
	"io"
	"iter"
	"os"
	"path/filepath"
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
		map[string]model.LLM{"web-researcher": m}, nil,
		vetting.NewJudgeFactory(m, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, nil)
	return orchestrator.New(sessions, traced, "You are the orchestrator.", planner, ex, nil, nil, nil)
}

// TestUserTurns_FromProductionShapedBundle is the #617 regression that
// matters most: UserTurns() must recover the user's turn from a bundle built
// the way a live run actually records one - root events stamped with ChatID
// only, filed by the real exporter into this chat's own FSStore file.
func TestUserTurns_FromProductionShapedBundle(t *testing.T) {
	dir := t.TempDir()
	store, err := ledger.NewFSStore(filepath.Join(dir, "ledger"))
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}

	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store))))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	const chatID = "prod-shaped-chat"
	const userMsg = "are ducks birds?"
	o := newOrchForTest(t, answerStub{text: "Ducks are birds."})
	for _, runErr := range o.Run(context.Background(), "u", chatID, userMsg, nil) {
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
	}

	rc, err := store.ReadStream(context.Background(), chatID)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("no ledger entries recorded under this chat's own stream - root events fell into \"unscoped\" instead")
	}

	bundlePath := filepath.Join(dir, "entries.jsonl")
	if err := os.WriteFile(bundlePath, data, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	sess, err := replay.Load(bundlePath)
	if err != nil {
		t.Fatalf("replay.Load: %v", err)
	}

	turns := sess.UserTurns()
	if len(turns) == 0 {
		t.Fatal("UserTurns() returned no turns - the chat's own bundle has no root stream (StreamKey{}) for eval to read")
	}
	if turns[0] != userMsg {
		t.Errorf("UserTurns()[0] = %q, want %q", turns[0], userMsg)
	}
}

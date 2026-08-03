package inference

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/replay"
)

// writeReplayFixture writes a minimal one-entry ledger JSONL a *replay.Session
// can Load, recording one "worker-model" chat call in node "node-a".
func writeReplayFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	line := `{"timestamp":"2026-01-01T00:00:00Z","attributes":{` +
		`"gen_ai.operation.name":"chat","gen_ai.request.model":"worker-model",` +
		`"quack.node":"node-a","gen_ai.agent.name":"worker","quack.round":"worker-r0",` +
		`"gen_ai.output.messages":"{\"role\":\"model\",\"parts\":[{\"text\":\"recorded answer\"}]}"` +
		`}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func req() *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}}
}

// TestReplayModel_StrictMiss_NoLiveFallback pins replay-strict's guarantee
// (.quack/replay-log.md "Forbidden": never a live call): NewReplayModel (no
// live delegate) yields the ForkSignal-shaped divergence as a plain error,
// never reaching a network call, even for a model name the recording never made.
func TestReplayModel_StrictMiss_NoLiveFallback(t *testing.T) {
	sess, err := replay.Load(writeReplayFixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := NewReplayModel(sess, "worker-model")
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"})

	// Consume the one recorded entry.
	for range m.GenerateContent(ctx, req(), false) {
	}
	// Second call has nothing recorded - strict mode must fail, not go live.
	var gotErr error
	for _, e := range m.GenerateContent(ctx, req(), false) {
		gotErr = e
	}
	if gotErr == nil {
		t.Fatal("want an error for a call the recording never made")
	}
	var fs *replay.ForkSignal
	if errors.As(gotErr, &fs) {
		t.Fatalf("strict mode returned a satisfiable ForkSignal (%v) - it must fail outright, never fork", fs)
	}
}

// TestReplayModelFork_MissFallsThroughToLive: fork mode's whole point - a
// call the recording can't satisfy is answered by the live delegate instead
// of failing.
func TestReplayModelFork_MissFallsThroughToLive(t *testing.T) {
	sess, err := replay.Load(writeReplayFixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("") // fork purely on divergence
	live := &stubModel{name: "worker-model", resps: []*model.LLMResponse{
		{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "LIVE answer"}}}, TurnComplete: true},
	}}
	m := NewReplayModelFork(sess, "worker-model", live)
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"})

	// Consume the one recorded entry (replays normally).
	var first *model.LLMResponse
	for r, e := range m.GenerateContent(ctx, req(), false) {
		if e != nil {
			t.Fatalf("first call: %v", e)
		}
		first = r
	}
	if first == nil || first.Content.Parts[0].Text != "recorded answer" {
		t.Fatalf("first call = %+v, want the recorded answer", first)
	}

	// Second call has nothing recorded - fork mode falls through to live.
	var second *model.LLMResponse
	var gotErr error
	for r, e := range m.GenerateContent(ctx, req(), false) {
		if e != nil {
			gotErr = e
		}
		second = r
	}
	if gotErr != nil {
		t.Fatalf("second call: %v, want it to fall through to live", gotErr)
	}
	if second == nil || second.Content.Parts[0].Text != "LIVE answer" {
		t.Fatalf("second call = %+v, want the live delegate's answer", second)
	}
}

// TestReplayModelFork_NoLiveDelegateConfigured: a fork-mode session with a
// nil live delegate (a caller that forgot to wire one) fails loudly, never
// panics and never silently drops the call.
func TestReplayModelFork_NoLiveDelegateConfigured(t *testing.T) {
	sess, err := replay.Load(writeReplayFixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sess.EnableFork("node-a") // forces node-a live from its first call
	m := NewReplayModelFork(sess, "worker-model", nil)
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Node: "node-a", Agent: "worker", Round: "worker-r0"})

	var gotErr error
	for _, e := range m.GenerateContent(ctx, req(), false) {
		gotErr = e
	}
	if gotErr == nil {
		t.Fatal("want an error when fork mode has no live delegate")
	}
}

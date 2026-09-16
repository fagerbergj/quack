package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// writeChatReplayFixture writes a one llm.call ledger JSONL with no recorded
// prompt provenance (pre-P1 shape) - enough for replay.Load and PromptSource's
// eager resolution (an empty resolved map) to succeed without a real bundle.
func writeChatReplayFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	line := `{"seq":1,"chat_id":"c","kind":"llm.call","at":"2026-01-01T00:00:00Z","payload":{` +
		`"request_model":"any-model"` +
		`}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReplayPromptSource_NonReplayConfig is #1422's "not a replay" case:
// buildFromConfig's normal boot path must get a nil Source, exactly P1's
// behaviour, when no provider is kind "replay".
func TestReplayPromptSource_NonReplayConfig(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"p": {Kind: "openai"}}}
	src, err := replayPromptSource(context.Background(), cfg)
	if err != nil || src != nil {
		t.Fatalf("replayPromptSource(non-replay) = %v, %v; want nil, nil", src, err)
	}
}

// TestReplayPromptSource_ReplayConfig loads the bundle a replay provider
// names and builds a Source over it - no error for a bundle with no recorded
// prompt provenance (pre-P1 entries fall through to normal resolution).
func TestReplayPromptSource_ReplayConfig(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"replay-test": {Kind: "replay", Bundle: writeChatReplayFixture(t)},
	}}
	src, err := replayPromptSource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("replayPromptSource: %v", err)
	}
	if src == nil {
		t.Fatal("replayPromptSource(replay config) = nil Source, want non-nil")
	}
	if _, ok, gerr := src.Get(context.Background(), "system/anything"); ok || gerr != nil {
		t.Fatalf("Get(unrecorded name) = ok=%v err=%v, want ok=false err=nil", ok, gerr)
	}
}

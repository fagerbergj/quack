package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestReplayPromptSource_DifferentBundlesRefuses is L3: replayifyProviders
// always sets every provider to the SAME bundle, so two different ones is a
// broken config - refuse rather than silently picking the first.
func TestReplayPromptSource_DifferentBundlesRefuses(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"a": {Kind: "replay", Bundle: writeChatReplayFixture(t)},
		"b": {Kind: "replay", Bundle: writeChatReplayFixture(t)},
	}}
	_, err := replayPromptSource(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "different bundles") {
		t.Fatalf("err = %v, want containing %q", err, "different bundles")
	}
}

// writeLangfusePromptReplayFixture writes one llm.call recorded from a named
// langfuse store, for testing replayPromptSource's langfuse.Client wiring (L4).
func writeLangfusePromptReplayFixture(t *testing.T, storeName, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	line := `{"seq":1,"chat_id":"c","node_id":"n","agent":"reviewer","round":"worker-r0","kind":"llm.call","at":"2026-01-01T00:00:00Z","payload":{` +
		`"request_model":"any-model","prompt_name":"reviewer","prompt_artifact":"system/code-reviewer",` +
		`"prompt_source":"` + storeName + `","prompt_version_id":"` + version + `"` +
		`}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReplayPromptSource_LangfuseStoreConfigured is L4: prompts.store set to
// a langfuse store fetches the recorded version from it.
func TestReplayPromptSource_LangfuseStoreConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("version"); got != "3" {
			t.Errorf("version = %q, want 3", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"system/code-reviewer","version":3,"type":"text","prompt":"be nice","config":{},"labels":[],"tags":[],"commitMessage":""}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"replay-test": {Kind: "replay", Bundle: writeLangfusePromptReplayFixture(t, "langfuse", "3")},
		},
		Prompts: config.PromptsConfig{Store: "langfuse"},
		Stores: map[string]config.StoreConfig{
			"langfuse": {Kind: "langfuse", URL: srv.URL, PublicKey: "pub", SecretKey: "secret"},
		},
	}
	src, err := replayPromptSource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("replayPromptSource: %v", err)
	}
	art, ok, gerr := src.Get(context.Background(), "system/code-reviewer")
	if gerr != nil || !ok {
		t.Fatalf("Get(system/code-reviewer) = ok=%v err=%v, want ok=true err=nil", ok, gerr)
	}
	if art.Body != "be nice" {
		t.Fatalf("art.Body = %q, want %q", art.Body, "be nice")
	}
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// TestReplayifyProviders_Strict: every named provider becomes kind:"replay"
// pointing at the bundle, with no fork_mode/live - the hermetic default
// (.quack/replay-log.md "replay-strict never makes a live call").
func TestReplayifyProviders_Strict(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"default": {Kind: "openai", Endpoint: "http://real", APIKey: "k"},
		"judge":   {Kind: "openai", Endpoint: "http://real2", APIKey: "k2"},
	}}
	replayifyProviders(cfg, "/tmp/bundle.zip", "")

	for name, p := range cfg.Providers {
		if p.Kind != "replay" || p.Bundle != "/tmp/bundle.zip" {
			t.Errorf("provider %q = %+v, want kind:replay bundle:/tmp/bundle.zip", name, p)
		}
		if p.ForkMode != "" || p.Live != nil {
			t.Errorf("provider %q = %+v, want no fork_mode/live in strict mode", name, p)
		}
	}
}

// TestReplayifyProviders_Fork: --fork-from carries EVERY provider's ORIGINAL
// (real) config forward as its `live` delegate - inference.NewModel's
// kind:"replay" + fork_mode:"fork" case builds the live model straight from
// it (factory.go), so a fork run needs no separate provider config beyond
// what quack.yaml already has.
func TestReplayifyProviders_Fork(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"default": {Kind: "openai", Endpoint: "http://real", APIKey: "k"},
	}}
	replayifyProviders(cfg, "/tmp/bundle.zip", "node-a")

	p := cfg.Providers["default"]
	if p.Kind != "replay" || p.ForkMode != "fork" || p.ForkFrom != "node-a" {
		t.Fatalf("provider = %+v, want kind:replay fork_mode:fork fork_from:node-a", p)
	}
	if p.Live == nil || p.Live.Kind != "openai" || p.Live.Endpoint != "http://real" || p.Live.APIKey != "k" {
		t.Errorf("Live = %+v, want the ORIGINAL real provider config", p.Live)
	}
}

// TestResolveBundle_LocalFile: an existing local path is used as-is, with a
// no-op cleanup.
func TestResolveBundle_LocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.zip")
	if err := os.WriteFile(path, []byte("fake zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, cleanup, err := resolveBundle(context.Background(), "", path)
	if err != nil {
		t.Fatalf("resolveBundle: %v", err)
	}
	if got != path {
		t.Errorf("got %q, want the local path unchanged (%q)", got, path)
	}
	cleanup() // must not remove the caller's own file
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("cleanup removed the caller-owned local file: %v", statErr)
	}
}

// TestResolveBundle_FetchesChatIDFromServer: an argument that ISN'T a local
// file is treated as a chat id and fetched from sourceServer's recording
// endpoint into a temp file, which cleanup then removes.
func TestResolveBundle_FetchesChatIDFromServer(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/zip")
		w.Write([]byte("the bundle bytes"))
	}))
	defer srv.Close()

	path, cleanup, err := resolveBundle(context.Background(), srv.URL, "chat-abc123")
	if err != nil {
		t.Fatalf("resolveBundle: %v", err)
	}
	defer cleanup()
	if gotPath != "/api/v1/chats/chat-abc123/recording" {
		t.Errorf("fetched path = %q, want the recording endpoint", gotPath)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fetched bundle: %v", err)
	}
	if string(b) != "the bundle bytes" {
		t.Errorf("bundle contents = %q, want the fetched bytes", b)
	}

	cleanup()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("cleanup did not remove the temp bundle file")
	}
}

// TestResolveBundle_FetchFailure: a chat id the server has no recording for
// (404 / ErrNotFound) surfaces as a clear error, not a panic on an empty body.
func TestResolveBundle_FetchFailure(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := resolveBundle(context.Background(), srv.URL, "no-such-chat")
	if err == nil {
		t.Fatal("want an error for a chat id with no recording")
	}
}

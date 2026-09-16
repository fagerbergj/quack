package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/artifactsrc"
)

// TestBundlePromptResolvesPerRound: the #1420 acceptance case - editing an
// agent's prompt.md on disk changes the next round, with no restart and no
// reload of the bundle.
func TestBundlePromptResolvesPerRound(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := filepath.Join("agents", "code-reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("agent-card.json", `{"name":"code-reviewer","description":"reviews"}`)
	write("prompt.md", "FIRST PROMPT\n")

	ctx := context.Background()
	res := artifactsrc.New("", nil, time.Nanosecond) // no TTL to wait out
	b, err := LoadBundle(ctx, res, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if b.Prompt != "FIRST PROMPT" {
		t.Fatalf("loaded prompt = %q", b.Prompt)
	}

	write("prompt.md", "SECOND PROMPT\n")
	got := b.ResolvePrompt(ctx, res)
	if got.Body != "SECOND PROMPT\n" {
		t.Errorf("round prompt = %q, want the edited file", got.Body)
	}
	if got.VersionID == b.PromptVersion {
		t.Error("version id did not move with the edited body")
	}
	if got.Source != artifactsrc.StaticSource {
		t.Errorf("source = %q, want static", got.Source)
	}
	// A blank prompt.md is a lost file, not an edit - the round keeps running.
	write("prompt.md", "   \n")
	if kept := b.ResolvePrompt(ctx, res); strings.TrimSpace(kept.Body) == "" {
		t.Error("a blank prompt.md was pinned; the round would run with no instruction")
	}
}

// TestOutOfTreeBundleHasVersionID: a bundle outside agents/ has no artifact
// name, but its bytes are still hashed - otherwise the ledger records no
// version and the assembled-prompt cache key never moves.
func TestOutOfTreeBundleHasVersionID(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := filepath.Join("vendor-bundles", "custom")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"agent-card.json": `{"name":"custom","description":"vendored"}`,
		"prompt.md":       "CUSTOM PROMPT\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if artifactsrc.BundleName("system", dir) != "" {
		t.Fatal("setup: this bundle must have no artifact name")
	}
	b, err := LoadBundle(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.PromptVersion) != 12 || b.PromptSource != artifactsrc.StaticSource {
		t.Errorf("provenance = %q/%q, want static plus a content hash", b.PromptSource, b.PromptVersion)
	}
}

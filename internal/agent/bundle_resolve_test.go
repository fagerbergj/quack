package agent

import (
	"context"
	"os"
	"path/filepath"
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
}

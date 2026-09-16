package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeBundle(t *testing.T, card, prompt string) string {
	t.Helper()
	dir := t.TempDir()
	if card != "" {
		if err := os.WriteFile(filepath.Join(dir, cardFile), []byte(card), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if prompt != "" {
		if err := os.WriteFile(filepath.Join(dir, promptFile), []byte(prompt), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadBundleOK(t *testing.T) {
	dir := writeBundle(t,
		`{"name":"web-researcher","description":"Researches the web.","skills":[{"id":"search","name":"Search","description":"finds pages"}]}`,
		"You are a web researcher.\n")
	b, err := LoadBundle(context.Background(), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.Card.Name != "web-researcher" {
		t.Errorf("name = %q, want %q", b.Card.Name, "web-researcher")
	}
	if b.Prompt != "You are a web researcher." {
		t.Errorf("prompt = %q (should be trimmed)", b.Prompt)
	}
	if len(b.Card.Skills) != 1 || b.Card.Skills[0].ID != "search" {
		t.Errorf("skills = %+v, want one skill id=search", b.Card.Skills)
	}
}

// TestShippedWebResearcherBundle guards the real bundle that ships in the repo:
// it must stay valid JSON with a name + non-empty prompt.
func TestShippedWebResearcherBundle(t *testing.T) {
	b, err := LoadBundle(context.Background(), nil, "../../agents/web-researcher")
	if err != nil {
		t.Fatal(err)
	}
	if b.Card.Name != "web-researcher" {
		t.Errorf("name = %q, want web-researcher", b.Card.Name)
	}
	if len(b.Card.Skills) == 0 {
		t.Error("expected the shipped card to declare skills")
	}
}

// TestBundlePromptArtifact guards H2's bug class: PromptArtifact (#1422) must
// be the resolved "system/<dir>" artifact name, derived from Dir - not
// Card.Name or any other caller-supplied key - and PinPrompt's own boot
// artifact must carry that same name, not the raw Dir path.
func TestBundlePromptArtifact(t *testing.T) {
	// bundledir resolves relative to the repo root (embedded fallback), so this
	// must be the "agents/<x>" shape BundleName expects, unlike the disk-relative
	// "../../agents/web-researcher" other tests in this file use.
	b, err := LoadBundle(context.Background(), nil, "agents/web-researcher")
	if err != nil {
		t.Fatal(err)
	}
	if b.PromptArtifact != "system/web-researcher" {
		t.Errorf("PromptArtifact = %q, want system/web-researcher", b.PromptArtifact)
	}
	if boot := b.PinPrompt(nil).Get(); boot.Name != "system/web-researcher" {
		t.Errorf("PinPrompt boot artifact Name = %q, want system/web-researcher (not Dir)", boot.Name)
	}
}

func TestLoadBundleErrors(t *testing.T) {
	cases := map[string]struct{ card, prompt string }{
		"missing card":   {"", "prompt"},
		"missing prompt": {`{"name":"x"}`, ""},
		"empty name":     {`{"name":"  "}`, "prompt"},
		"empty prompt":   {`{"name":"x"}`, "   \n  "},
		"bad json":       {`{not json}`, "prompt"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadBundle(context.Background(), nil, writeBundle(t, c.card, c.prompt)); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

// TestLoadBundleHash: stable across two loads of the same files, changes
// when prompt.md changes (#1096 ledger provenance).
func TestLoadBundleHash(t *testing.T) {
	card := `{"name":"x","description":"d"}`
	dir := writeBundle(t, card, "prompt one")
	b1, err := LoadBundle(context.Background(), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := LoadBundle(context.Background(), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if b1.Hash != b2.Hash {
		t.Errorf("hash not stable across loads: %q vs %q", b1.Hash, b2.Hash)
	}

	dir2 := writeBundle(t, card, "prompt two")
	b3, err := LoadBundle(context.Background(), nil, dir2)
	if err != nil {
		t.Fatal(err)
	}
	if b3.Hash == b1.Hash {
		t.Error("hash did not change when prompt.md changed")
	}
}

func TestLoadBundleMemory(t *testing.T) {
	dir := writeBundle(t, `{"name":"x","description":"d"}`, "prompt")

	// Absent → "".
	if got, err := LoadBundleMemory(context.Background(), nil, dir); err != nil || got != "" {
		t.Fatalf("absent memory.md = (%q, %v), want (\"\", nil)", got, err)
	}

	// Present → trimmed content.
	if err := os.WriteFile(filepath.Join(dir, memoryFile), []byte("  ## What to remember\nstuff\n  "), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadBundleMemory(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("LoadBundleMemory: %v", err)
	}
	if got != "## What to remember\nstuff" {
		t.Fatalf("memory.md content = %q", got)
	}
}

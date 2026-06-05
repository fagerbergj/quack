package agent

import (
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
	b, err := LoadBundle(dir)
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
	b, err := LoadBundle("../../agents/web-researcher")
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
			if _, err := LoadBundle(writeBundle(t, c.card, c.prompt)); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

package docstore

import (
	"strings"
	"testing"
)

func TestChunkMarkdownPacksSections(t *testing.T) {
	// Small sections pack into one chunk; a heading starts a section.
	doc := "intro line\n\n# A\nalpha\n\n# B\nbeta\n"
	chunks := chunkMarkdown(doc, 1000, 100)
	if len(chunks) != 1 {
		t.Fatalf("small doc should be one chunk, got %d: %q", len(chunks), chunks)
	}
	for _, want := range []string{"intro line", "# A", "# B"} {
		if !strings.Contains(chunks[0], want) {
			t.Errorf("chunk missing %q", want)
		}
	}
}

func TestChunkMarkdownWindowsOversized(t *testing.T) {
	// A single section larger than maxSize is split into overlapping windows.
	body := strings.Repeat("x", 50)
	chunks := chunkMarkdown("# H\n"+body, 20, 5)
	if len(chunks) < 2 {
		t.Fatalf("oversized section should window into >1 chunk, got %d", len(chunks))
	}
	for _, c := range chunks {
		if runeLen(c) > 20 {
			t.Errorf("chunk exceeds maxSize: %d runes", runeLen(c))
		}
	}
}

func TestChunkTextOverlap(t *testing.T) {
	chunks := chunkText("abcdefghij", 4, 2) // step 2
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	if chunks[0] != "abcd" {
		t.Errorf("first window = %q, want abcd", chunks[0])
	}
	// Blank-only input yields nothing.
	if got := chunkText("   ", 4, 2); got != nil {
		t.Errorf("blank input = %v, want nil", got)
	}
}

func TestIsHeading(t *testing.T) {
	for _, h := range []string{"# x", "### y", "###### z"} {
		if !isHeading(h) {
			t.Errorf("%q should be a heading", h)
		}
	}
	for _, n := range []string{"#nospace", "text", "####### too many", ""} {
		if isHeading(n) {
			t.Errorf("%q should not be a heading", n)
		}
	}
}

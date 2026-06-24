package memory

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// fakeModel is a consolidator stub that replies with a fixed text (canned JSON ops).
type fakeModel struct{ reply string }

func (fakeModel) Name() string { return "fake-consolidator" }

func (f fakeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: f.reply}}}}, nil)
	}
}

func TestCommit_AddThenRecall(t *testing.T) {
	ctx := context.Background()
	consolidator := fakeModel{reply: "```json\n{\"ops\":[{\"action\":\"ADD\",\"content\":\"transportforireland.ie is authoritative for Irish transit\",\"kind\":\"source\"}]}\n```"}
	s := newSQLiteStore(t, "task", consolidator)

	n, err := s.Commit(ctx, "u1", "web-researcher",
		[]Candidate{{Content: "use the official transit site", Metadata: map[string]string{"kind": "source"}}},
		"Dublin buses run by transportforireland.ie ...")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 1 {
		t.Fatalf("Commit wrote %d, want 1", n)
	}

	// Task memory is scoped to the agent (author), so recall passes AppName.
	resp, err := s.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "irish transit", AppName: "web-researcher", UserID: "u1"})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("recall got %d, want 1 committed memory", len(resp.Memories))
	}
}

// TestCommit_Noop verifies the gate's vetting drop: when the consolidator returns
// no ops (nothing worth keeping), nothing is written.
func TestCommit_Noop(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", fakeModel{reply: `{"ops":[]}`})

	n, err := s.Commit(ctx, "u1", "web-researcher", []Candidate{{Content: "today's bus fare is 2 euro"}}, "")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 0 {
		t.Fatalf("Commit wrote %d, want 0 (vetting should have dropped it)", n)
	}
}

// TestCommit_NoConsolidator guards the read-only-store error path (no LLM call).
func TestCommit_NoConsolidator(t *testing.T) {
	s := &Store{} // no consolidator
	if _, err := s.Commit(context.Background(), "u1", "a", []Candidate{{Content: "x"}}, ""); err == nil {
		t.Fatal("Commit with nil consolidator should error")
	}
}

func TestNeighbourProbe(t *testing.T) {
	// Short input passes through untouched.
	if got := neighbourProbe("hello", nil); got != "hello" {
		t.Fatalf("short probe = %q, want hello", got)
	}

	// A long source answer is capped to maxProbeRunes.
	long := strings.Repeat("x", maxProbeRunes*3)
	if got := neighbourProbe(long, nil); len([]rune(got)) != maxProbeRunes {
		t.Fatalf("probe len = %d, want %d", len([]rune(got)), maxProbeRunes)
	}

	// Staged content leads, so it survives truncation even with a huge answer.
	got := neighbourProbe(strings.Repeat("y", maxProbeRunes*3), []Candidate{{Content: "STAGED-FACT"}})
	if !strings.HasPrefix(got, "STAGED-FACT") {
		t.Fatalf("probe must lead with staged content, got %q...", got[:20])
	}
	if len([]rune(got)) != maxProbeRunes {
		t.Fatalf("capped probe len = %d, want %d", len([]rune(got)), maxProbeRunes)
	}
}

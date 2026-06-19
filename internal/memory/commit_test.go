package memory

import (
	"context"
	"iter"
	"os"
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
	addr := os.Getenv("QDRANT_URL")
	if addr == "" {
		t.Skip("QDRANT_URL not set; skipping qdrant integration test")
	}
	ctx := context.Background()
	const coll = "quack_test_commit"

	consolidator := fakeModel{reply: "```json\n{\"ops\":[{\"action\":\"ADD\",\"content\":\"transportforireland.ie is authoritative for Irish transit\",\"kind\":\"source\"}]}\n```"}
	s, err := Open(ctx, addr, fakeEmbedder{}, consolidator, coll, 5)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.client.DeleteCollection(ctx, coll) })

	n, err := s.Commit(ctx, "u1", "web-researcher",
		[]Candidate{{Content: "use the official transit site", Metadata: map[string]string{"kind": "source"}}},
		"Dublin buses run by transportforireland.ie ...")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n != 1 {
		t.Fatalf("Commit wrote %d, want 1", n)
	}

	resp, err := s.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "irish transit", UserID: "u1"})
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
	addr := os.Getenv("QDRANT_URL")
	if addr == "" {
		t.Skip("QDRANT_URL not set; skipping qdrant integration test")
	}
	ctx := context.Background()
	const coll = "quack_test_commit_noop"

	s, err := Open(ctx, addr, fakeEmbedder{}, fakeModel{reply: `{"ops":[]}`}, coll, 5)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.client.DeleteCollection(ctx, coll) })

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

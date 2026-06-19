package memory

import (
	"context"
	"os"
	"testing"

	"github.com/qdrant/go-client/qdrant"
	adkmemory "google.golang.org/adk/memory"
)

// fakeEmbedder returns a fixed unit vector for every text, so any query matches
// any stored point (cosine = 1). Enough to exercise the round-trip + payload
// mapping + per-user filter without a real embedding model.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// TestStore_RecallRoundTrip needs a live Qdrant (QDRANT_URL). It creates a throwaway
// collection, upserts a point, and checks SearchMemory recalls it and honours the
// per-user filter.
func TestStore_RecallRoundTrip(t *testing.T) {
	addr := os.Getenv("QDRANT_URL")
	if addr == "" {
		t.Skip("QDRANT_URL not set; skipping qdrant integration test")
	}
	ctx := context.Background()
	const coll = "quack_test_memory"

	s, err := Open(ctx, addr, fakeEmbedder{}, coll, 5)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.client.DeleteCollection(ctx, coll) })

	// Empty collection → no hits, no error.
	resp, err := s.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "anything", UserID: "u1"})
	if err != nil {
		t.Fatalf("SearchMemory (empty): %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("empty collection returned %d memories, want 0", len(resp.Memories))
	}

	// Upsert one point for user u1.
	wait := true
	if _, err := s.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: coll,
		Wait:           &wait,
		Points: []*qdrant.PointStruct{{
			Id:      qdrant.NewIDNum(1),
			Vectors: qdrant.NewVectorsDense([]float32{1, 0, 0, 0}),
			Payload: qdrant.NewValueMap(map[string]any{
				payloadContent: "transportforireland.ie is authoritative for Irish transit",
				payloadUserID:  "u1",
				payloadAuthor:  "web-researcher",
			}),
		}},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// u1 recalls it.
	resp, err = s.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "irish transit sources", UserID: "u1"})
	if err != nil {
		t.Fatalf("SearchMemory (u1): %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("u1 got %d memories, want 1", len(resp.Memories))
	}
	got := resp.Memories[0]
	if got.Content == nil || len(got.Content.Parts) == 0 || got.Content.Parts[0].Text == "" {
		t.Fatalf("recalled memory has no content text")
	}
	if got.Author != "web-researcher" {
		t.Fatalf("author = %q, want web-researcher", got.Author)
	}

	// u2 sees nothing — per-user isolation.
	resp, err = s.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "irish transit sources", UserID: "u2"})
	if err != nil {
		t.Fatalf("SearchMemory (u2): %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("u2 got %d memories, want 0 (user filter leaked)", len(resp.Memories))
	}
}

package memory

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/model"
)

// fakeEmbedder returns a fixed unit vector for every text, so any query matches
// any stored point (cosine = 1). Enough to exercise the round-trip + scope filter
// without a real embedding model.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// newSQLiteStore builds a Store backed by an embedded sqlite file in a temp dir -
// the always-on backend for unit tests (no container). The shared Store logic
// (scope, recall, consolidation) is identical to the qdrant backend.
func newSQLiteStore(t *testing.T, domain string, consolidator model.LLM) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mem.db")
	s, err := OpenSQLite(context.Background(), path, fakeEmbedder{}, consolidator, "test_"+domain, domain, 5, 0.5)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return s
}

// upsertScoped writes one fixed-vector point under the given bucket, so the
// fakeEmbedder makes any query match and only the bucket filter decides. Used to
// plant LEGACY (pre-bucket, agent-name-scoped) points too - see scope_test.go.
func upsertScoped(t *testing.T, s *Store, id, scope, content string) {
	t.Helper()
	if err := s.idx.upsert(context.Background(), []point{{
		ID: id, Vector: []float32{1, 0, 0, 0}, Content: content, Scope: scope, Author: "web-researcher",
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

// countingEmbedder records how many times Embed was actually invoked.
type countingEmbedder struct{ calls int }

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.calls++
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 2, 3}
	}
	return out, nil
}

func TestEmbedMemoizesSingleInputs(t *testing.T) {
	ce := &countingEmbedder{}
	s := &Store{embedder: ce, embCache: newEmbedCache(512), log: slog.Default()}
	ctx := context.Background()

	for i := 0; i < 5; i++ { // same query re-embedded (mimics preload per turn)
		if _, err := s.embed(ctx, []string{"node task"}, "recall"); err != nil {
			t.Fatalf("embed: %v", err)
		}
	}
	if _, err := s.embed(ctx, []string{"different task"}, "recall"); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if ce.calls != 2 {
		t.Fatalf("embedder invoked %d times, want 2 (one per distinct text; the rest cached)", ce.calls)
	}
}

func TestEmbedCacheBoundedClears(t *testing.T) {
	c := newEmbedCache(2)
	c.put("a", []float32{1})
	c.put("b", []float32{2})
	c.put("c", []float32{3}) // exceeds cap → clear, then store c
	if _, ok := c.get("a"); ok {
		t.Fatal("expected clear-on-full to drop old entries")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("newest entry should be present after clear")
	}
}

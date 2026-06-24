package memory

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/model"
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

// newSQLiteStore builds a Store backed by an embedded sqlite file in a temp dir —
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

// upsertScoped writes one fixed-vector point under the given scope, so the
// fakeEmbedder makes any query match and only the scope filter decides.
func upsertScoped(t *testing.T, s *Store, id, scope, content string) {
	t.Helper()
	if err := s.idx.upsert(context.Background(), []point{{
		ID: id, Vector: []float32{1, 0, 0, 0}, Content: content, Scope: scope, Author: "web-researcher",
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

// TestStore_TaskScopedByAgent proves task memory is keyed by the AGENT
// (req.AppName), not the per-request A2A UserID — so a memory written in one
// request is recalled in another despite a different "A2A_USER_<ctxid>", and a
// different agent can't see it. Regression guard for the bug where every
// invocation got its own user ID and recall was always empty.
func TestStore_TaskScopedByAgent(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	// Empty collection → no hits, no error.
	resp, err := s.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "anything", AppName: "web-researcher"})
	if err != nil {
		t.Fatalf("SearchMemory (empty): %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("empty collection returned %d memories, want 0", len(resp.Memories))
	}

	// Stored under the agent-name scope (what Commit writes for the task domain).
	upsertScoped(t, s, "1", "web-researcher", "transportforireland.ie is authoritative for Irish transit")

	// The SAME agent, across two DIFFERENT per-request A2A users, both recall it.
	for _, volatileUser := range []string{"A2A_USER_req1", "A2A_USER_req2"} {
		resp, err = s.SearchMemory(ctx, &adkmemory.SearchRequest{
			Query: "irish transit sources", AppName: "web-researcher", UserID: volatileUser,
		})
		if err != nil {
			t.Fatalf("SearchMemory (%s): %v", volatileUser, err)
		}
		if len(resp.Memories) != 1 {
			t.Fatalf("agent recall with user %s got %d memories, want 1 (recall must not depend on the A2A user)", volatileUser, len(resp.Memories))
		}
		if got := resp.Memories[0]; got.Content == nil || len(got.Content.Parts) == 0 || got.Content.Parts[0].Text == "" {
			t.Fatalf("recalled memory has no content text")
		}
	}

	// A DIFFERENT agent sees nothing — tradecraft is partitioned per agent.
	resp, err = s.SearchMemory(ctx, &adkmemory.SearchRequest{
		Query: "irish transit sources", AppName: "synthesizer", UserID: "A2A_USER_req1",
	})
	if err != nil {
		t.Fatalf("SearchMemory (other agent): %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("other agent got %d memories, want 0 (agent partition leaked)", len(resp.Memories))
	}
}

// TestStore_UserScopedByUserID proves user memory still keys by the real userID
// (the orchestrator isn't behind A2A, so its userID is stable), and ignores AppName.
func TestStore_UserScopedByUserID(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "user", nil)

	upsertScoped(t, s, "1", "local", "the user keeps bees")

	// Right user recalls it (AppName is irrelevant for the user domain).
	resp, err := s.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "about the user", UserID: "local", AppName: "orchestrator"})
	if err != nil {
		t.Fatalf("SearchMemory (local): %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("user local got %d memories, want 1", len(resp.Memories))
	}

	// A different user sees nothing — personal memory stays isolated.
	resp, err = s.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "about the user", UserID: "someone-else"})
	if err != nil {
		t.Fatalf("SearchMemory (other user): %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("other user got %d memories, want 0 (user isolation leaked)", len(resp.Memories))
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

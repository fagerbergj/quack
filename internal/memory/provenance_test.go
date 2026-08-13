package memory

import (
	"context"
	"testing"
)

// TestCommitProvenance covers memory-lifecycle Phase 1 (design doc, issue #849):
// every write is stamped with who minted it, and an UPDATE never re-mints.
func TestCommitProvenance(t *testing.T) {
	const fixedNow = "2026-08-12T00:00:00Z"
	orig := nowRFC3339
	nowRFC3339 = func() string { return fixedNow }
	t.Cleanup(func() { nowRFC3339 = orig })

	const bucket = "repo:test-repo"
	sc := Scope{Repo: "test-repo"}

	t.Run("ADD stamps provenance and mints", func(t *testing.T) {
		ctx := context.Background()
		s := newSQLiteStore(t, "task", fakeModel{reply: `{"ops":[{"action":"ADD","content":"new fact","kind":"convention"}]}`})
		prov := Provenance{ChatID: "chat-1", NodeID: "node-1", Source: "github"}

		if _, err := s.Commit(ctx, sc, "author", prov, nil, "some answer"); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		pts, err := s.idx.query(ctx, []string{bucket}, []float32{1, 0, 0, 0}, 5)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(pts) != 1 {
			t.Fatalf("got %d points, want 1", len(pts))
		}
		got := pts[0]
		if got.ChatID != "chat-1" || got.NodeID != "node-1" || got.Source != "github" {
			t.Fatalf("provenance = %+v, want chat-1/node-1/github", got)
		}
		if got.MintedAt != fixedNow {
			t.Fatalf("MintedAt = %q, want %q (set on ADD)", got.MintedAt, fixedNow)
		}
		if got.Timestamp != fixedNow {
			t.Fatalf("Timestamp = %q, want %q", got.Timestamp, fixedNow)
		}
	})

	t.Run("UPDATE preserves the original mint", func(t *testing.T) {
		ctx := context.Background()
		s := newSQLiteStore(t, "task", nil)
		if err := s.idx.upsert(ctx, []point{{
			ID: "mem-1", Vector: []float32{1, 0, 0, 0}, Content: "old content", Scope: bucket,
			Author: "author", Timestamp: "2020-01-01T00:00:00Z",
			ChatID: "old-chat", NodeID: "old-node", Source: "old-source", MintedAt: "2020-01-01T00:00:00Z",
		}}); err != nil {
			t.Fatalf("seed upsert: %v", err)
		}
		s.consolidator = fakeModel{reply: `{"ops":[{"action":"UPDATE","id":"mem-1","content":"corrected content","kind":"convention"}]}`}

		// A different run touches it - a wording correction, not a re-mint.
		prov := Provenance{ChatID: "new-chat", NodeID: "new-node", Source: "new-source"}
		if _, err := s.Commit(ctx, sc, "author", prov, nil, "some answer"); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		pts, err := s.idx.query(ctx, []string{bucket}, []float32{1, 0, 0, 0}, 5)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(pts) != 1 {
			t.Fatalf("got %d points, want 1 (UPDATE overwrites mem-1 in place)", len(pts))
		}
		got := pts[0]
		if got.Content != "corrected content" {
			t.Fatalf("Content = %q, want the UPDATE's new content", got.Content)
		}
		if got.ChatID != "old-chat" || got.NodeID != "old-node" || got.Source != "old-source" {
			t.Fatalf("provenance = %+v, want the ORIGINAL mint (old-chat/old-node/old-source) kept", got)
		}
		if got.MintedAt != "2020-01-01T00:00:00Z" {
			t.Fatalf("MintedAt = %q, want the original mint time unchanged", got.MintedAt)
		}
		if got.Timestamp != fixedNow {
			t.Fatalf("Timestamp = %q, want it to move to this commit's time (last-touched)", got.Timestamp)
		}
	})

	t.Run("zero Provenance stamps empty strings without error", func(t *testing.T) {
		ctx := context.Background()
		s := newSQLiteStore(t, "task", fakeModel{reply: `{"ops":[{"action":"ADD","content":"native run fact","kind":"convention"}]}`})

		if _, err := s.Commit(ctx, sc, "author", Provenance{}, nil, "some answer"); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		pts, err := s.idx.query(ctx, []string{bucket}, []float32{1, 0, 0, 0}, 5)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(pts) != 1 {
			t.Fatalf("got %d points, want 1", len(pts))
		}
		got := pts[0]
		if got.ChatID != "" || got.NodeID != "" || got.Source != "" {
			t.Fatalf("provenance = %+v, want all empty (native quack run)", got)
		}
		if got.MintedAt == "" {
			t.Fatal("MintedAt must still be stamped even with a zero Provenance")
		}
	})
}

package docstore

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// TestNew covers the kind factory without a database: empty url and unknown kind
// both error before any connection is attempted.
func TestNew(t *testing.T) {
	if _, err := New("", ""); err == nil {
		t.Error("empty url should error")
	}
	if _, err := New("bogus", "x"); err == nil {
		t.Error("unknown kind should error")
	}
}

// TestPostgresRoundTrip exercises the reference adapter against a real Postgres.
// Skips when DATABASE_URL is unset (mirrors the memory integration tests).
func TestPostgresRoundTrip(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset; skipping docstore integration test")
	}
	s, err := New("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	d := Document{
		ID: uuid.NewString(), ContentHash: uuid.NewString(),
		Title: "Title", Content: "cleaned text", Summary: "abstract",
		Tags: []string{"alpha", "beta"}, Series: "s", DateMonth: "2026-06",
	}
	if err := s.Create(ctx, d); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, ok, err := s.Get(ctx, d.ID)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Title != "Title" || got.Content != "cleaned text" || !slices.Equal(got.Tags, []string{"alpha", "beta"}) {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if byHash, ok, err := s.GetByHash(ctx, d.ContentHash); err != nil || !ok || byHash.ID != d.ID {
		t.Errorf("get by hash: ok=%v err=%v id=%q", ok, err, byHash.ID)
	}

	d.Title = "Updated"
	if err := s.Update(ctx, d); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _, _ := s.Get(ctx, d.ID); got.Title != "Updated" {
		t.Errorf("update not persisted: title=%q", got.Title)
	}

	if _, ok, err := s.Get(ctx, "does-not-exist"); err != nil || ok {
		t.Errorf("missing lookup: ok=%v err=%v, want (false,nil)", ok, err)
	}
}

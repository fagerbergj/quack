package memory

import (
	"context"
	"fmt"
	"testing"
)

// TestRescope_ApplyMatchesDryRunAcrossPages is the review's regression case
// for #1263's finding #1: role:coding shrinks as points move out of it under
// apply, so pagination by offset over that SAME bucket must not skip the
// tail once the eligible set spans more than one List page (DefaultListLimit
// = 50). 100 resolvable points + 20 with no provenance, on sqlite.
func TestRescope_ApplyMatchesDryRunAcrossPages(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, t.TempDir()+"/mem.db", fixedTestEmbedder{}, nil, "test_rescope_scale", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	const resolvable, noProvenance = 100, 20
	var pts []point
	for i := 0; i < resolvable; i++ {
		pts = append(pts, point{
			ID: fmt.Sprintf("gh-%d", i), Vector: []float32{1, 0, 0, 0}, Content: fmt.Sprintf("fact %d", i),
			Scope: prefixed(bucketRole, RoleCoding), ChatID: "gh-chat", Timestamp: nowRFC3339(), MintedAt: nowRFC3339(),
		})
	}
	for i := 0; i < noProvenance; i++ {
		pts = append(pts, point{
			ID: fmt.Sprintf("noprov-%d", i), Vector: []float32{1, 0, 0, 0}, Content: fmt.Sprintf("orphan %d", i),
			Scope: prefixed(bucketRole, RoleCoding), Timestamp: nowRFC3339(), MintedAt: nowRFC3339(),
		})
	}
	if err := s.idx.upsert(ctx, pts); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	resolve := func(_ context.Context, chatID string) (string, bool) {
		if chatID == "gh-chat" {
			return "github.com/acme/games", true
		}
		return "", false
	}

	dry, err := s.Rescope(ctx, resolve, false)
	if err != nil {
		t.Fatalf("dry Rescope: %v", err)
	}
	if got := dry.ByRepo["github.com/acme/games"]; got == nil || got.Count != resolvable {
		t.Fatalf("dry run tally = %+v, want %d", dry.ByRepo["github.com/acme/games"], resolvable)
	}
	if dry.SkippedNoProvenance != noProvenance {
		t.Fatalf("dry run SkippedNoProvenance = %d, want %d", dry.SkippedNoProvenance, noProvenance)
	}

	// Dry run must not have written anything.
	_, total, err := s.List(ctx, []string{prefixed(bucketRole, RoleCoding)}, 0, 0, false)
	if err != nil {
		t.Fatalf("List after dry run: %v", err)
	}
	if total != resolvable+noProvenance {
		t.Fatalf("role:coding after dry run = %d, want %d (nothing moved)", total, resolvable+noProvenance)
	}

	apply, err := s.Rescope(ctx, resolve, true)
	if err != nil {
		t.Fatalf("apply Rescope: %v", err)
	}
	if got := apply.ByRepo["github.com/acme/games"]; got == nil || got.Count != dry.ByRepo["github.com/acme/games"].Count {
		t.Fatalf("apply tally = %+v, want it to match the dry run tally %+v", apply.ByRepo["github.com/acme/games"], dry.ByRepo["github.com/acme/games"])
	}

	_, movedTotal, err := s.List(ctx, []string{prefixed(bucketRepo, "github.com/acme/games")}, 0, 0, false)
	if err != nil {
		t.Fatalf("List repo bucket: %v", err)
	}
	if movedTotal != resolvable {
		t.Fatalf("repo:github.com/acme/games has %d points, want all %d moved", movedTotal, resolvable)
	}
	_, leftTotal, err := s.List(ctx, []string{prefixed(bucketRole, RoleCoding)}, 0, 0, false)
	if err != nil {
		t.Fatalf("List role bucket after apply: %v", err)
	}
	if leftTotal != noProvenance {
		t.Fatalf("role:coding after apply = %d, want only the %d no-provenance points left", leftTotal, noProvenance)
	}
}

type fixedTestEmbedder struct{}

func (fixedTestEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

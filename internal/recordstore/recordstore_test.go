package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"google.golang.org/adk/v2/artifact"
)

type doc struct {
	A string `json:"a"`
	B int    `json:"b"`
}

// hintIdentity: instance = hint verbatim - stands in for a subject-identity
// kind like code_review (its instance comes from outside the content).
func hintIdentity(_ []byte, hint string) (string, error) { return hint, nil }

func init() {
	Register("test.structured", KindSpec{
		Class: Structured,
		Validate: func(raw json.RawMessage) error {
			var d doc
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			if d.A == "" {
				return errors.New("a is required")
			}
			return nil
		},
		Identity: hintIdentity,
	})
	Register("test.blob", KindSpec{Class: Blob, Identity: hintIdentity})
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	return New(artifact.InMemoryService(), "quack", "user1", "chat1")
}

func TestKindOf(t *testing.T) {
	if got := KindOf("code_review:pr:123"); got != "code_review" {
		t.Fatalf("KindOf = %q, want code_review", got)
	}
	// instance itself contains the separator - KindOf must still isolate the kind.
	if got := KindOf("finding:3f9a2c1e"); got != "finding" {
		t.Fatalf("KindOf(finding) = %q", got)
	}
}

func TestSaveStructuredRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev, err := c.SaveStructured(ctx, "test.structured", doc{A: "x", B: 1}, "main", Lineage{NodeID: "n1", Round: 1})
	if err != nil || rev != 1 || id != "test.structured:main" {
		t.Fatalf("SaveStructured: id=%q rev=%d err=%v", id, rev, err)
	}
	raw, _, lineage, gotRev, ok, err := c.LatestWithMeta(ctx, id)
	if err != nil || !ok || gotRev != 1 {
		t.Fatalf("LatestWithMeta: raw=%s rev=%d ok=%v err=%v", raw, gotRev, ok, err)
	}
	// InMemoryService has no row to persist lineage on (#1090 known ceiling) -
	// zero value, not an error.
	if lineage.NodeID != "" {
		t.Fatalf("expected zero lineage over InMemoryService, got %+v", lineage)
	}
}

func TestSaveStructuredRejectsInvalidBody(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	if _, _, err := c.SaveStructured(ctx, "test.structured", doc{A: "", B: 1}, "main", Lineage{}); err == nil {
		t.Fatal("expected validation error for missing required field")
	}
	if _, _, ok, _ := c.Latest(ctx, "test.structured:main"); ok {
		t.Fatal("a rejected save must not persist a revision")
	}
}

func TestSaveStructuredRejectsWrongClass(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	if _, _, err := c.SaveStructured(ctx, "test.blob", doc{A: "x"}, "main", Lineage{}); err == nil {
		t.Fatal("expected error saving a blob kind via SaveStructured")
	}
}

func TestSaveBlobAcceptsAnyMime(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, _, err := c.SaveBlob(ctx, "test.blob", []byte("# hi"), "text/markdown", "doc:1", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if id != "test.blob:doc:1" {
		t.Fatalf("id = %q", id)
	}
	raw, _, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || string(raw) != "# hi" {
		t.Fatalf("raw=%s ok=%v err=%v", raw, ok, err)
	}
}

func TestSaveUnregisteredKindFails(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	if _, _, err := c.SaveStructured(ctx, "nope", doc{A: "x"}, "main", Lineage{}); err == nil {
		t.Fatal("expected error for an unregistered kind")
	}
}

func TestIdentityForMatchesSave(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	wantID, err := IdentityFor("test.structured", doc{A: "x"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	gotID, _, err := c.SaveStructured(ctx, "test.structured", doc{A: "x"}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if wantID != gotID {
		t.Fatalf("IdentityFor = %q, Save derived %q", wantID, gotID)
	}
}

func TestLatestMissing(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	_, _, ok, err := c.Latest(ctx, "test.blob:nope")
	if err != nil || ok {
		t.Fatalf("expected no record, got ok=%v err=%v", ok, err)
	}
}

// TestEveryRevisionKept proves design V4.1 #2: no retention call exists, so
// every save keeps its own revision and Latest always reports the newest.
func TestEveryRevisionKept(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	var id string
	for i := 0; i < 5; i++ {
		var err error
		id, _, err = c.SaveBlob(ctx, "test.blob", []byte{byte(i)}, "text/plain", "doc:1", Lineage{})
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, rev, ok, err := c.Latest(ctx, id)
	if err != nil || !ok || rev != 5 {
		t.Fatalf("Latest: raw=%v rev=%d ok=%v err=%v", raw, rev, ok, err)
	}
	if got := raw[0]; got != 4 {
		t.Fatalf("Latest returned revision content %d, want the 5th save's byte 4", got)
	}
}

// TestSameContentDifferentHintSameID: a content-hashed kind's identity
// ignores hint, matching the finding requirement (same finding, any node).
func TestContentHashIgnoresHint(t *testing.T) {
	Register("test.hashed", KindSpec{
		Class: Blob,
		Identity: func(content []byte, _ string) (string, error) {
			return string(content), nil // trivial "hash" for the test
		},
	})
	ctx := context.Background()
	c := newTestClient(t)
	id1, _, err := c.SaveBlob(ctx, "test.hashed", []byte("same"), "text/plain", "node-a", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	id2, _, err := c.SaveBlob(ctx, "test.hashed", []byte("same"), "text/plain", "node-b", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("same content under different hints produced different ids: %q vs %q", id1, id2)
	}
}

// TestEditDirectApply covers the base_revision-current case: edits apply straight to the latest content.
func TestEditDirectApply(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	rev2, merged, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"world"`}}, Lineage{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if rev2 != rev1+1 {
		t.Fatalf("revision = %d, want %d", rev2, rev1+1)
	}
	var d doc
	if err := json.Unmarshal(merged, &d); err != nil || d.A != "world" {
		t.Fatalf("merged = %s, err=%v", merged, err)
	}
}

// TestEditStaleBaseMergesWhenUnique covers V4 §7 case 5: a stale
// base_revision still succeeds when its `old` snippets still match uniquely
// against the newer latest content (a non-intersecting concurrent edit).
func TestEditStaleBaseMergesWhenUnique(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent editor bumps B while this caller still thinks rev1 is latest.
	rev2, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"b":1`, New: `"b":2`}}, Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	// This caller's base_revision (rev1) is now stale, but its edit targets
	// a region the concurrent edit never touched - must still succeed.
	rev3, merged, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"world"`}}, Lineage{})
	if err != nil {
		t.Fatalf("Edit with stale base should merge, got: %v", err)
	}
	if rev3 != rev2+1 {
		t.Fatalf("revision = %d, want %d", rev3, rev2+1)
	}
	var d doc
	if err := json.Unmarshal(merged, &d); err != nil || d.A != "world" || d.B != 2 {
		t.Fatalf("merged = %s (want both edits applied), err=%v", merged, err)
	}
}

// TestEditStaleBaseConflictsWhenIntersecting covers V4 §7 case 5's failure
// half: a stale edit whose `old` region was itself changed by the newer
// revision no longer matches, and the call fails with the current latest -
// no partial write.
func TestEditStaleBaseConflictsWhenIntersecting(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	id, rev1, err := c.SaveStructured(ctx, "test.structured", doc{A: "hello", B: 1}, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"changed"`}}, Lineage{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Edit(ctx, id, rev1, []EditOp{{Old: `"a":"hello"`, New: `"a":"conflicting"`}}, Lineage{})
	var conflict *EditConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("Edit = %v, want *EditConflict", err)
	}
	if conflict.Revision != rev1+1 {
		t.Fatalf("conflict.Revision = %d, want %d", conflict.Revision, rev1+1)
	}
	if _, _, _, latestRev, _, _ := c.LatestWithMeta(ctx, id); latestRev != rev1+1 {
		t.Fatalf("failed edit must not write - latest revision = %d, want %d", latestRev, rev1+1)
	}
}

// TestEditNonUniqueMatchFails: an `old` matching 0 or 2+ times fails without
// writing, even against the current latest revision (no stale base involved).
func TestEditNonUniqueMatchFails(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	blobID, brev, err := c.SaveBlob(ctx, "test.blob", []byte("aa"), "text/plain", "b1", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Edit(ctx, blobID, brev, []EditOp{{Old: "a", New: "z"}}, Lineage{}); err == nil {
		t.Fatal("Edit with a 2x-matching old should fail")
	}
	if _, _, _, latestRev, _, _ := c.LatestWithMeta(ctx, blobID); latestRev != brev {
		t.Fatalf("failed edit must not write - latest revision = %d, want %d", latestRev, brev)
	}
}

// TestWriteFindingOffSchemaFailsWithoutWriting: an off-schema structured
// write is rejected by the registry's Validate and nothing is persisted.
func TestWriteFindingOffSchemaFailsWithoutWriting(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	// test.structured requires non-empty "a" - the write_<kind> equivalent
	// for this suite's stand-in kind.
	if _, _, err := c.SaveStructured(ctx, "test.structured", map[string]any{"b": 1}, "bad", Lineage{}); err == nil {
		t.Fatal("expected validation failure for a body missing the required field")
	}
	if _, _, ok, _ := c.Latest(ctx, "test.structured:bad"); ok {
		t.Fatal("an off-schema write must not persist a revision")
	}
}

// TestIdentityForMatchesWriteID: the id a write returns equals what
// IdentityFor (the registry's own Identity function) derives for the same
// content/hint - required by V4 §7 case 5.
func TestIdentityForMatchesWriteID(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	d := doc{A: "x", B: 1}
	wantID, err := IdentityFor("test.structured", d, "main")
	if err != nil {
		t.Fatal(err)
	}
	gotID, _, err := c.SaveStructured(ctx, "test.structured", d, "main", Lineage{})
	if err != nil {
		t.Fatal(err)
	}
	if gotID != wantID {
		t.Fatalf("SaveStructured id = %q, want IdentityFor's %q", gotID, wantID)
	}
}

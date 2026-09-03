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
	raw, lineage, gotRev, ok, err := c.LatestWithMeta(ctx, id)
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

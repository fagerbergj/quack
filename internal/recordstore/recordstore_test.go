package recordstore

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/artifact"
)

type doc struct {
	A string `json:"a"`
	B int    `json:"b"`
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	return New(artifact.InMemoryService(), "quack", "user1", "chat1")
}

func TestSaveJSONRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	rev, err := c.SaveJSON(ctx, "review", doc{A: "x", B: 1})
	if err != nil || rev != 1 {
		t.Fatalf("SaveJSON: rev=%d err=%v", rev, err)
	}
	raw, gotRev, ok, err := c.Latest(ctx, "review")
	if err != nil || !ok || gotRev != 1 {
		t.Fatalf("Latest: raw=%s rev=%d ok=%v err=%v", raw, gotRev, ok, err)
	}
	if _, err := c.SaveJSON(ctx, "review", doc{A: "y", B: 2}); err != nil {
		t.Fatal(err)
	}
	_, gotRev, ok, err = c.Latest(ctx, "review")
	if err != nil || !ok || gotRev != 2 {
		t.Fatalf("Latest after 2nd save: rev=%d ok=%v err=%v", gotRev, ok, err)
	}
	v1, err := c.LoadVersion(ctx, "review", 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(v1) == "" {
		t.Fatal("LoadVersion(1) empty")
	}
}

func TestLatestMissing(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	_, _, ok, err := c.Latest(ctx, "nope")
	if err != nil || ok {
		t.Fatalf("expected no record, got ok=%v err=%v", ok, err)
	}
}

func TestKeepLastRevisions(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	for i := 0; i < 5; i++ {
		if _, err := c.SaveJSON(ctx, "body", doc{A: "s", B: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.KeepLastRevisions(ctx, "body", 3); err != nil {
		t.Fatal(err)
	}
	raw, rev, ok, err := c.Latest(ctx, "body")
	if err != nil || !ok || rev != 5 {
		t.Fatalf("Latest after retention: raw=%s rev=%d ok=%v err=%v", raw, rev, ok, err)
	}
	if v, err := c.LoadVersion(ctx, "body", 1); err != nil {
		t.Fatal(err)
	} else if v != nil {
		t.Fatalf("expected version 1 evicted, got %s", v)
	}
}

func TestDiff(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	if _, err := c.SaveJSON(ctx, "review", map[string]any{"findings": []int{1}, "clean": []string{"a.go"}}); err != nil {
		t.Fatal(err)
	}
	statuses, err := c.Diff(ctx, "review", 0, map[string]any{
		"findings": []int{1},
		"clean":    []string{"a.go", "b.go"},
		"critique": []int{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if statuses["findings"] != Unchanged {
		t.Errorf("findings: want Unchanged, got %s", statuses["findings"])
	}
	if statuses["clean"] != Changed {
		t.Errorf("clean: want Changed, got %s", statuses["clean"])
	}
	if statuses["critique"] != Added {
		t.Errorf("critique: want Added, got %s", statuses["critique"])
	}
}

func TestDeleteAll(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	if _, err := c.SaveJSON(ctx, "review", doc{A: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAll(ctx, "review"); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := c.Latest(ctx, "review")
	if err != nil || ok {
		t.Fatalf("expected deleted, ok=%v err=%v", ok, err)
	}
}

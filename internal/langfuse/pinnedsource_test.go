package langfuse

import (
	"context"
	"net/http"
	"testing"

	"github.com/fagerbergj/quack/internal/artifactsrc"
)

func TestPinnedSource_GetsExactVersion(t *testing.T) {
	var gotQuery string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"name":"system/code-reviewer","version":3,"type":"text","prompt":"pinned body"}`))
	})
	src := &PinnedSource{Client: c, Pins: map[string]int{"system/code-reviewer": 3}}

	art, ok, err := src.Get(context.Background(), "system/code-reviewer")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if art.Body != "pinned body" || art.VersionID != "3" {
		t.Fatalf("art = %+v", art)
	}
	if gotQuery != "version=3" {
		t.Fatalf("query = %q, want version=3", gotQuery)
	}
}

func TestPinnedSource_SeedIsANoOp(t *testing.T) {
	src := &PinnedSource{}
	if err := src.Seed(context.Background(), "system/code-reviewer", artifactsrc.Artifact{}); err != nil {
		t.Fatalf("Seed must never error: %v", err)
	}
}

func TestPinnedSource_UnpinnedNameFallsThrough(t *testing.T) {
	src := &PinnedSource{Client: nil, Pins: map[string]int{"system/other": 1}}
	_, ok, err := src.Get(context.Background(), "system/code-reviewer")
	if err != nil || ok {
		t.Fatalf("want (false, nil) for an unpinned name, got ok=%v err=%v", ok, err)
	}
}

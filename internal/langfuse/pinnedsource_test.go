package langfuse

import (
	"context"
	"errors"
	"net/http"
	"strings"
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

// TestPinnedSource_PinnedVersion404IsHardError pins suggestion 7: a pinned
// name whose version 404s must return an error naming it, not the (false,
// nil) miss shape ChainSource would silently fall through to the store on.
func TestPinnedSource_PinnedVersion404IsHardError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	src := &PinnedSource{Client: c, Pins: map[string]int{"system/code-reviewer": 9}}
	_, ok, err := src.Get(context.Background(), "system/code-reviewer")
	if ok {
		t.Fatalf("Get on a 404'd pin: ok = true, want false")
	}
	if err == nil || !strings.Contains(err.Error(), "system/code-reviewer@9") {
		t.Fatalf("Get on a 404'd pin: err = %v, want an error naming name@version", err)
	}
	if !errors.Is(err, artifactsrc.ErrHard) {
		t.Fatalf("Get on a 404'd pin: err = %v, want it to wrap artifactsrc.ErrHard", err)
	}
}

// TestPinnedSource_PinnedVersion404PropagatesThroughResolver pins suggestion
// 7 end to end: artifactsrc.New(Chain(PinnedSource, ...)).Resolve must return
// the pin-miss error, not silently fall back to the shipped static artifact
// (PR #1444 round-2 finding).
func TestPinnedSource_PinnedVersion404PropagatesThroughResolver(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	src := &PinnedSource{Client: c, Pins: map[string]int{"system/code-reviewer": 9}}
	res := artifactsrc.New("langfuse", artifactsrc.Chain(src, nil), 0)

	_, err := res.Resolve(context.Background(), "system/code-reviewer")
	if !errors.Is(err, artifactsrc.ErrHard) {
		t.Fatalf("Resolve = %v, want the pin-miss error propagated, not a static fallback", err)
	}
}

func TestPinnedSource_UnpinnedNameFallsThrough(t *testing.T) {
	src := &PinnedSource{Client: nil, Pins: map[string]int{"system/other": 1}}
	_, ok, err := src.Get(context.Background(), "system/code-reviewer")
	if err != nil || ok {
		t.Fatalf("want (false, nil) for an unpinned name, got ok=%v err=%v", ok, err)
	}
}

func TestPinnedSource_ResolveNow(t *testing.T) {
	ok := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"system/code-reviewer","version":3,"type":"text","prompt":"pinned body"}`))
	})
	if err := (&PinnedSource{Client: ok, Pins: map[string]int{"system/code-reviewer": 3}}).
		ResolveNow(context.Background(), "system/code-reviewer"); err != nil {
		t.Fatalf("ResolveNow: %v", err)
	}

	notFound := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	err := (&PinnedSource{Client: notFound, Pins: map[string]int{"system/code-reviewer": 9}}).
		ResolveNow(context.Background(), "system/code-reviewer")
	if err == nil || !strings.Contains(err.Error(), "system/code-reviewer@9") {
		t.Fatalf("ResolveNow on a 404 = %v, want an error naming name@version", err)
	}
}

func TestParsePin(t *testing.T) {
	name, v, err := ParsePin("system/code-reviewer@7")
	if err != nil || name != "system/code-reviewer" || v != 7 {
		t.Fatalf("got %q %d %v", name, v, err)
	}
	for _, bad := range []string{"system/x", "@3", "system/x@0", "system/x@seven", ""} {
		if _, _, err := ParsePin(bad); err == nil {
			t.Errorf("ParsePin(%q) = nil error, want error", bad)
		}
	}
}

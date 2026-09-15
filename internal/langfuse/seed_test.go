package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestSeedCreatesOn404(t *testing.T) {
	var posted map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&posted)
		writeJSON(w, promptWire{Name: "system/foo", Version: 1, Type: "text", Prompt: rawString("static")})
	})
	action, err := Seed(context.Background(), c, "system/foo", "static", "hash1")
	if err != nil || action != "created" {
		t.Fatalf("action=%q err=%v", action, err)
	}
	if posted["commitMessage"] != "quack-seed hash1" || posted["labels"] != nil {
		t.Fatalf("posted = %+v", posted)
	}
}

func TestSeedUpdatesWhenSeededHashDiffers(t *testing.T) {
	var posted map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, promptWire{Name: "system/foo", Version: 2, Type: "text", Prompt: rawString("old"), CommitMessage: "quack-seed hash1"})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&posted)
		writeJSON(w, promptWire{Name: "system/foo", Version: 3, Type: "text", Prompt: rawString("new")})
	})
	action, err := Seed(context.Background(), c, "system/foo", "new", "hash2")
	if err != nil || action != "updated" {
		t.Fatalf("action=%q err=%v", action, err)
	}
	if posted["commitMessage"] != "quack-seed hash2" {
		t.Fatalf("posted = %+v", posted)
	}
}

func TestSeedUnchangedWhenSeededHashSame(t *testing.T) {
	posts := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, promptWire{Name: "system/foo", Version: 2, Type: "text", Prompt: rawString("static"), CommitMessage: "quack-seed hash1"})
			return
		}
		posts++
		writeJSON(w, promptWire{})
	})
	action, err := Seed(context.Background(), c, "system/foo", "static", "hash1")
	if err != nil || action != "unchanged" {
		t.Fatalf("action=%q err=%v", action, err)
	}
	if posts != 0 {
		t.Fatalf("expected no POST, got %d", posts)
	}
}

func TestSeedOperatorEditedNeverTouched(t *testing.T) {
	posts := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, promptWire{Name: "system/foo", Version: 5, Type: "text", Prompt: rawString("operator wrote this"), CommitMessage: "tweak wording"})
			return
		}
		posts++
		writeJSON(w, promptWire{})
	})
	action, err := Seed(context.Background(), c, "system/foo", "static", "hash1")
	if err != nil || action != "operator-edited" {
		t.Fatalf("action=%q err=%v", action, err)
	}
	if posts != 0 {
		t.Fatalf("expected no POST, got %d", posts)
	}
}

func TestResolvePinnedLabel(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("label") != "production" {
			t.Fatalf("expected label=production, got %q", r.URL.RawQuery)
		}
		writeJSON(w, promptWire{Name: "x", Version: 4, Type: "text", Prompt: rawString("pinned")})
	})
	p, found, err := Resolve(context.Background(), c, "x", "production")
	if err != nil || !found || p.Body != "pinned" {
		t.Fatalf("p=%+v found=%v err=%v", p, found, err)
	}
}

func TestResolveFallsBackToLatest(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		label := r.URL.Query().Get("label")
		if label == "production" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if label != "latest" {
			t.Fatalf("expected label=latest fallback, got %q", label)
		}
		writeJSON(w, promptWire{Name: "x", Version: 7, Type: "text", Prompt: rawString("latest")})
	})
	p, found, err := Resolve(context.Background(), c, "x", "production")
	if err != nil || !found || p.Body != "latest" || calls != 2 {
		t.Fatalf("p=%+v found=%v err=%v calls=%d", p, found, err, calls)
	}
}

func TestResolveNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, found, err := Resolve(context.Background(), c, "x", "production")
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

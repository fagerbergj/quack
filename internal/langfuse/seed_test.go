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
		rawPrompt(`{"name": "system/foo", "version": 1, "type": "text", "prompt": "static"}`)(w, r)
	})
	action, err := c.Seed(context.Background(), "system/foo", "static", "hash1")
	if err != nil || action != Created {
		t.Fatalf("action=%q err=%v", action, err)
	}
	if posted["commitMessage"] != "quack-seed hash1" {
		t.Fatalf("posted = %+v", posted)
	}
	labels, ok := posted["labels"].([]any)
	if !ok || len(labels) != 0 {
		t.Fatalf("labels = %#v, want empty array (Langfuse assigns latest itself; no production label)", posted["labels"])
	}
}

func TestSeedUpdatesWhenSeededHashDiffers(t *testing.T) {
	var posted map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			rawPrompt(`{"name": "system/foo", "version": 2, "type": "text", "prompt": "old", "tags": ["quack-seed"], "commitMessage": "quack-seed hash1"}`)(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&posted)
		rawPrompt(`{"name": "system/foo", "version": 3, "type": "text", "prompt": "new"}`)(w, r)
	})
	action, err := c.Seed(context.Background(), "system/foo", "new", "hash2")
	if err != nil || action != Updated {
		t.Fatalf("action=%q err=%v", action, err)
	}
	if posted["commitMessage"] != "quack-seed hash2" {
		t.Fatalf("posted = %+v", posted)
	}
}

func TestSeedUpdatePreservesOperatorTags(t *testing.T) {
	var posted map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			rawPrompt(`{"name": "system/foo", "version": 2, "type": "text", "prompt": "old", "tags": ["quack-seed", "reviewed"], "commitMessage": "quack-seed hash1"}`)(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&posted)
		rawPrompt(`{"name": "system/foo", "version": 3, "type": "text", "prompt": "new"}`)(w, r)
	})
	if _, err := c.Seed(context.Background(), "system/foo", "new", "hash2"); err != nil {
		t.Fatal(err)
	}
	tags, ok := posted["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "quack-seed" || tags[1] != "reviewed" {
		t.Fatalf("tags = %#v, want [quack-seed reviewed] (union, not overwrite)", posted["tags"])
	}
}

func TestSeedUnchangedWhenSeededHashSame(t *testing.T) {
	posts := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			rawPrompt(`{"name": "system/foo", "version": 2, "type": "text", "prompt": "static", "commitMessage": "quack-seed hash1"}`)(w, r)
			return
		}
		posts++
		rawPrompt(`{}`)(w, r)
	})
	action, err := c.Seed(context.Background(), "system/foo", "static", "hash1")
	if err != nil || action != Unchanged {
		t.Fatalf("action=%q err=%v", action, err)
	}
	if posts != 0 {
		t.Fatalf("expected no POST, got %d", posts)
	}
}

// TestSeedUnchangedTrimsHashWhitespace guards against a stray newline in the
// stored commit message forcing a spurious new seeded version on every boot.
func TestSeedUnchangedTrimsHashWhitespace(t *testing.T) {
	posts := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			rawPrompt(`{"name": "system/foo", "version": 2, "type": "text", "prompt": "static", "commitMessage": "quack-seed hash1\n"}`)(w, r)
			return
		}
		posts++
		rawPrompt(`{}`)(w, r)
	})
	action, err := c.Seed(context.Background(), "system/foo", "static", "hash1")
	if err != nil || action != Unchanged {
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
			rawPrompt(`{"name": "system/foo", "version": 5, "type": "text", "prompt": "operator wrote this", "commitMessage": "tweak wording"}`)(w, r)
			return
		}
		posts++
		rawPrompt(`{}`)(w, r)
	})
	action, err := c.Seed(context.Background(), "system/foo", "static", "hash1")
	if err != nil || action != OperatorEdited {
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
		rawPrompt(`{"name": "x", "version": 4, "type": "text", "prompt": "pinned"}`)(w, r)
	})
	c.pinLabel = "production"
	p, found, err := c.Resolve(context.Background(), "x")
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
		rawPrompt(`{"name": "x", "version": 7, "type": "text", "prompt": "latest"}`)(w, r)
	})
	c.pinLabel = "production"
	p, found, err := c.Resolve(context.Background(), "x")
	if err != nil || !found || p.Body != "latest" || calls != 2 {
		t.Fatalf("p=%+v found=%v err=%v calls=%d", p, found, err, calls)
	}
}

func TestResolveDoesNotFallThroughOn500(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("label") == "production" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		t.Fatalf("should not fall through to latest on a 500, got label=%q", r.URL.Query().Get("label"))
	})
	c.pinLabel = "production"
	_, found, err := c.Resolve(context.Background(), "x")
	if err == nil || found {
		t.Fatalf("found=%v err=%v, want an error and no fallback", found, err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call (no fallback), got %d", calls)
	}
	if !IsTransient(err) {
		t.Fatalf("want transient error, got %v", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c.pinLabel = "production"
	_, found, err := c.Resolve(context.Background(), "x")
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

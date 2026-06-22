package tools

import (
	"net/http"
	"testing"
)

// The portability seam: kind selects the adapter, empty = default, unknown =
// error. This is what lets a backend be swapped via config alone.
func TestNewWebSearcher(t *testing.T) {
	c := &http.Client{}
	if _, err := newWebSearcher("", "http://searx", c); err != nil {
		t.Errorf("default kind: %v", err)
	}
	if _, err := newWebSearcher("searxng", "http://searx", c); err != nil {
		t.Errorf("searxng kind: %v", err)
	}
	if _, err := newWebSearcher("", "", c); err == nil {
		t.Error("empty backend URL should error")
	}
	if _, err := newWebSearcher("bogus", "http://x", c); err == nil {
		t.Error("unknown kind should error")
	}
}

func TestNewPageRenderer(t *testing.T) {
	c := &http.Client{}
	r, err := newPageRenderer("", "http://crawl", c)
	if err != nil || r == nil {
		t.Errorf("default kind: r=%v err=%v", r, err)
	}
	// An empty render backend is valid: no fallback renderer, not an error.
	if r, err := newPageRenderer("", "", c); err != nil || r != nil {
		t.Errorf("empty backend should yield (nil, nil); got r=%v err=%v", r, err)
	}
	if _, err := newPageRenderer("bogus", "http://x", c); err == nil {
		t.Error("unknown render kind should error")
	}
}

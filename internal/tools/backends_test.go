package tools

import (
	"net/http"
	"testing"
)

// The portability seam: kind selects the adapter, empty = default, unknown =
// error. This is what lets a backend be swapped via config alone.
func TestNewWebSearcher(t *testing.T) {
	c := &http.Client{}
	if _, err := newWebSearcher("", "http://searx", "", c); err != nil {
		t.Errorf("default kind: %v", err)
	}
	if _, err := newWebSearcher("searxng", "http://searx", "", c); err != nil {
		t.Errorf("searxng kind: %v", err)
	}
	if _, err := newWebSearcher("", "", "", c); err == nil {
		t.Error("empty searxng URL should error")
	}
	// exa needs neither URL nor key (keyless MCP fallback); a key opts into REST.
	if _, err := newWebSearcher("exa", "", "", c); err != nil {
		t.Errorf("exa without key should not error: %v", err)
	}
	if _, err := newWebSearcher("exa", "", "exa-key", c); err != nil {
		t.Errorf("exa with key should not error: %v", err)
	}
	if _, err := newWebSearcher("bogus", "http://x", "", c); err == nil {
		t.Error("unknown kind should error")
	}
}

func TestNewFetcher(t *testing.T) {
	c := &http.Client{}

	// Empty kind defaults to direct (plain GET, no backend needed).
	if f, err := newFetcher("", "", c); err != nil {
		t.Errorf("empty kind: err=%v", err)
	} else if _, ok := f.(directFetcher); !ok {
		t.Errorf("empty kind should give directFetcher, got %T", f)
	}
	if f, err := newFetcher("direct", "", c); err != nil {
		t.Errorf("direct: err=%v", err)
	} else if _, ok := f.(directFetcher); !ok {
		t.Errorf("kind direct should give directFetcher, got %T", f)
	}

	// crawl4ai needs a URL; with one it yields the render-capable impl.
	if _, err := newFetcher("crawl4ai", "", c); err == nil {
		t.Error("crawl4ai without a url should error")
	}
	if f, err := newFetcher("crawl4ai", "http://crawl", c); err != nil {
		t.Errorf("crawl4ai with url: err=%v", err)
	} else if _, ok := f.(crawl4aiFetcher); !ok {
		t.Errorf("kind crawl4ai should give crawl4aiFetcher, got %T", f)
	}

	if _, err := newFetcher("bogus", "http://x", c); err == nil {
		t.Error("unknown fetch kind should error")
	}
}

package tools

import (
	"net/http"
	"testing"
)

// Kind selects the adapter: empty = default, unknown = error.
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

func TestNewRenderer(t *testing.T) {
	c := &http.Client{}
	for _, kind := range []string{"", "direct"} {
		if r, err := newRenderer(kind, "", c); err != nil || r != nil {
			t.Errorf("kind %q: renderer=%v err=%v, want a plain GET with no renderer", kind, r, err)
		}
	}
	if _, err := newRenderer("crawl4ai", "", c); err == nil {
		t.Error("crawl4ai without a url should error")
	}
	if r, err := newRenderer("crawl4ai", "http://crawl", c); err != nil || r == nil {
		t.Errorf("crawl4ai with url: renderer=%v err=%v, want a renderer", r, err)
	}
	if _, err := newRenderer("bogus", "http://x", c); err == nil {
		t.Error("unknown fetch kind should error")
	}
}

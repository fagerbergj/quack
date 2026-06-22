package tools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Portability seam for tools backed by external software. Each such tool depends
// on a small interface (a "port"), and a kind-based factory selects the concrete
// adapter from config — so swapping the backend (a different search engine, a
// different render service) is a config change plus one new adapter, never a
// rewrite of the tool. Mirrors the inference.NewModel provider-kind factory.
//
// Rule (see .agents/skills/write-tool): any builtin tool that talks to external
// software must go through a port + config-selected adapter like this.

// WebSearcher returns web search hits for a query. note is a non-fatal warning
// for the agent (e.g. some backends were rate-limited); err is reserved for a
// search that produced nothing usable. Implemented by searxngSearcher.
type WebSearcher interface {
	Search(ctx context.Context, query string) (results []SearchResult, note string, err error)
}

// PageRenderer renders a JS-heavy or bot-walled page to readable Markdown using a
// real-browser backend, the fallback when a direct GET is too thin. Implemented
// by crawl4aiRenderer.
type PageRenderer interface {
	Render(ctx context.Context, url string) (markdown string, err error)
}

// Backend is a tool's external-backend binding, named by tool (not by
// implementation) so callers stay agnostic: Kind selects the adapter (empty =
// default) and URL is its endpoint.
type Backend struct {
	Kind string
	URL  string
}

// Backend kinds. Empty kind defaults to the only implemented adapter, so existing
// config keeps working without naming a kind.
const (
	backendSearXNG  = "searxng"
	backendCrawl4AI = "crawl4ai"
)

// newWebSearcher selects the web-search adapter for kind (default: searxng).
func newWebSearcher(kind, base string, client *http.Client) (WebSearcher, error) {
	if kind == "" {
		kind = backendSearXNG
	}
	switch kind {
	case backendSearXNG:
		if base == "" {
			return nil, fmt.Errorf("web_search requires a SearXNG backend URL")
		}
		return &searxngSearcher{client: client, base: strings.TrimRight(base, "/")}, nil
	default:
		return nil, fmt.Errorf("web_search: unknown backend kind %q", kind)
	}
}

// newPageRenderer selects the render adapter for kind (default: crawl4ai). An
// empty base means no render backend is configured: returns a nil renderer and
// the fetch tool simply skips the render fallback.
func newPageRenderer(kind, base string, client *http.Client) (PageRenderer, error) {
	if kind == "" {
		kind = backendCrawl4AI
	}
	switch kind {
	case backendCrawl4AI:
		if base == "" {
			return nil, nil // optional: no render fallback
		}
		return &crawl4aiRenderer{client: client, base: strings.TrimRight(base, "/")}, nil
	default:
		return nil, fmt.Errorf("web_fetch: unknown render backend kind %q", kind)
	}
}

package tools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Portability seam: kind-based factory selects adapters for tools backed by external software.
type WebSearcher interface {
	Search(ctx context.Context, query string) (results []SearchResult, note string, err error)
}

// PageRenderer renders JS-heavy pages to readable Markdown via a real-browser backend.
type PageRenderer interface {
	Render(ctx context.Context, url string) (markdown string, err error)
}

// Backend: external-backend binding - Kind selects adapter, URL is the endpoint.
type Backend struct {
	Kind string
	URL  string
	Key  string
}

// Backend kinds.
const (
	backendSearXNG  = "searxng"
	backendExa      = "exa"
	backendDirect   = "direct"
	backendCrawl4AI = "crawl4ai"
)

// newWebSearcher selects the web-search adapter for kind (default: searxng).
func newWebSearcher(kind, base, key string, client *http.Client) (WebSearcher, error) {
	if kind == "" {
		kind = backendSearXNG
	}
	switch kind {
	case backendSearXNG:
		if base == "" {
			return nil, fmt.Errorf("web_search requires a SearXNG backend URL")
		}
		return &searxngSearcher{client: client, base: strings.TrimRight(base, "/")}, nil
	case backendExa:
		// key set ⇒ Exa REST; empty ⇒ keyless hosted MCP; base is unused.
		return newExaSearcher(key, client), nil
	default:
		return nil, fmt.Errorf("web_search: unknown backend kind %q", kind)
	}
}

// web_fetch's adapter is selected by newFetcher in fetch.go.

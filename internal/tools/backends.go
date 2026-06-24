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
	Key  string // optional API key (e.g. web_search kind exa → Exa REST)
}

// Backend kinds. web_search defaults to searxng (its only adapter); web_fetch
// defaults to direct (a plain GET, no external service) and opts into crawl4ai.
const (
	backendSearXNG  = "searxng"
	backendExa      = "exa"
	backendDirect   = "direct"
	backendCrawl4AI = "crawl4ai"
)

// newWebSearcher selects the web-search adapter for kind (default: searxng). Both
// adapters satisfy the same port, so the agent's web_search tool is identical
// regardless of backend. exa is keyless (it speaks Exa's hosted MCP under the
// hood) and needs no URL; searxng needs its instance URL.
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
		// key set ⇒ Exa REST (JSON); empty ⇒ keyless hosted MCP. base (url) is
		// unused — Exa's endpoints are fixed.
		return newExaSearcher(key, client), nil
	default:
		return nil, fmt.Errorf("web_search: unknown backend kind %q", kind)
	}
}

// (web_fetch's adapter is selected by newFetcher in fetch.go, since both impls
// share that file's fetch engine.)

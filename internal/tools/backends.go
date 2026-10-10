package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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

const (
	backendSearXNG  = "searxng"
	backendExa      = "exa"
	backendDirect   = "direct"
	backendCrawl4AI = "crawl4ai"
)

// newWebSearcher (default searxng): keyed Exa falls back to keyless Exa, and Exa to SearXNG when base is set.
func newWebSearcher(kind, base, key string, client *http.Client) (WebSearcher, error) {
	if kind == "" {
		kind = backendSearXNG
	}
	base = strings.TrimRight(base, "/")
	switch kind {
	case backendSearXNG:
		if base == "" {
			return nil, fmt.Errorf("web_search requires a SearXNG backend URL")
		}
		return fallbackSearcher{{name: "searxng", s: &searxngSearcher{client: client, base: base}}}, nil
	case backendExa:
		var chain fallbackSearcher
		if key != "" {
			chain = append(chain, &searchBackend{name: "exa-rest", s: newExaSearcher(key, client)})
		}
		chain = append(chain, &searchBackend{name: "exa-keyless", s: newExaSearcher("", client)})
		if base != "" {
			chain = append(chain, &searchBackend{name: "searxng", s: &searxngSearcher{client: client, base: base}})
		}
		return chain, nil
	default:
		return nil, fmt.Errorf("web_search: unknown backend kind %q", kind)
	}
}

// searchBackend is one link of a fallback chain; failing marks an ongoing
// failure streak so it logs one WARN per streak, not one per query.
type searchBackend struct {
	name    string
	s       WebSearcher
	failing atomic.Bool
}

type fallbackSearcher []*searchBackend

func (f fallbackSearcher) Search(ctx context.Context, query string) ([]SearchResult, string, error) {
	var errs []error
	for _, b := range f {
		results, note, err := b.s.Search(ctx, query)
		if err == nil {
			if b.failing.Swap(false) {
				slog.Info("web_search: backend recovered", "component", "tools", "backend", b.name)
			}
			return results, note, nil
		}
		if !b.failing.Swap(true) {
			slog.Warn("web_search: backend failed; falling back to the next configured one", "component", "tools", "backend", b.name, "error", err)
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, "", errors.Join(errs...)
}

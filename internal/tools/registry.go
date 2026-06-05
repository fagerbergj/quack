// Package tools is Quack's registry of built-in agent tools (config `kind:
// builtin`). An agent's config selects tools by name; Build resolves those names
// to ADK tools, injecting shared dependencies (HTTP clients, backend URLs, a
// summarizer model). M1 ships web_search, web_fetch, and summarize.
package tools

import (
	"fmt"
	"net/http"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
)

// Deps are the shared dependencies injected into built-in tools.
type Deps struct {
	// Client is a plain HTTP client for trusted internal backends (SearXNG,
	// crawl4ai) that live on private network addresses.
	Client *http.Client
	// Guarded is an SSRF-protected HTTP client for fetching untrusted,
	// agent-chosen URLs. Defaults to GuardedClient().
	Guarded *http.Client
	// SearXNG is the base URL of the SearXNG search backend (keyless).
	SearXNG string
	// Crawl4AI is the base URL of the crawl4ai render backend (keyless), called
	// for its Markdown /md endpoint. Optional; when empty, fetch skips the render
	// fallback.
	Crawl4AI string
	// Summarizer is the model the summarize tool uses to condense text.
	Summarizer model.LLM
}

// constructor builds one tool from the shared dependencies.
type constructor func(Deps) (tool.Tool, error)

var registry = map[string]constructor{
	"web_search": newWebSearch,
	"web_fetch":  newFetch,
	"summarize":  newSummarize,
}

// Known reports whether name is a registered built-in tool. Used by config
// validation so an unknown tool fails fast at startup.
func Known(name string) bool {
	_, ok := registry[name]
	return ok
}

// Build resolves tool names to ADK tools, injecting d. Unknown names are an
// error (the extension seam for future tool kinds).
func Build(names []string, d Deps) ([]tool.Tool, error) {
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if d.Guarded == nil {
		d.Guarded = GuardedClient()
	}
	out := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		ctor, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("tools: unknown builtin tool %q", name)
		}
		t, err := ctor(d)
		if err != nil {
			return nil, fmt.Errorf("tools: build %q: %w", name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

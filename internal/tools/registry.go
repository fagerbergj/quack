// Package tools is Quack's registry of built-in agent tools (config `kind:
// builtin`). An agent's config selects tools by name; Build resolves those names
// to ADK tools, injecting shared dependencies (HTTP clients, backend URLs, a
// summarizer model).
package tools

import (
	"fmt"
	"net/http"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"

	"github.com/fagerbergj/quack/internal/docstore"
)

// Deps are the shared dependencies injected into built-in tools.
type Deps struct {
	// Client is a plain HTTP client for trusted internal backends that live on
	// private network addresses.
	Client *http.Client
	// Guarded is an SSRF-protected HTTP client for fetching untrusted,
	// agent-chosen URLs. Defaults to GuardedClient().
	Guarded *http.Client
	// WebSearch binds the web_search tool's backend (kind + endpoint).
	WebSearch Backend
	// Fetch binds the web_fetch tool's render backend (kind + endpoint).
	// Optional; an empty URL means fetch skips the render fallback.
	Fetch Backend
	// Summarizer is the model the summarize tool uses to condense text.
	Summarizer model.LLM
	// Cache is a shared response cache used by web_fetch and web_search to avoid
	// redundant network requests. Optional; when nil, caching is disabled.
	Cache *URLCache
	// DocStore backs the document record tools (load/create/update_document).
	// nil when documents aren't configured — those tools then refuse to build.
	DocStore docstore.DocStore
	// FTS is the full-text index for documents (search_document; create_document
	// also indexes into it). nil when no FTS backend is configured.
	FTS docstore.FTSIndex
	// Vector is the semantic index for documents (semantic_search_document;
	// create_document also indexes into it). nil when no vector backend is set.
	Vector docstore.VectorIndex
}

// constructor builds one tool from the shared dependencies.
type constructor func(Deps) (tool.Tool, error)

var registry = map[string]constructor{
	"web_search":               newWebSearch,
	"web_fetch":                newFetch,
	"summarize":                newSummarize,
	"current_date":             newCurrentDate,
	"stage_memory":             newStageMemory,
	"load_document":            newLoadDocument,
	"create_document":          newCreateDocument,
	"update_document":          newUpdateDocument,
	"search_document":          newSearchDocument,
	"semantic_search_document": newSemanticSearchDocument,
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

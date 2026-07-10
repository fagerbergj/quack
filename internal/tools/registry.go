// Package tools is Quack's registry of built-in agent tools (config `kind:
// builtin`). An agent's config selects tools by name; Build resolves those names
// to ADK tools, injecting shared dependencies (HTTP clients, backend URLs, a
// summarizer model).
package tools

import (
	"fmt"
	"net/http"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/workspace"
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
	// Advisor is the mentor agent ask_advisor consults. nil ⇒ the tool is never
	// registered (build skips it; see resolveToolNames in internal/serve).
	Advisor adkagent.Agent
	// Sessions is the shared session.Service ask_advisor uses to persist each
	// node's mentor conversation (same store as chat/plan sessions, a distinct
	// AppName — see ask_advisor.go).
	Sessions session.Service
	// Workspace is the filesystem jail the fs.go tools (read_file, write_file,
	// edit_file, list_dir, glob, grep, delete_path) resolve every path
	// through. nil ⇒ those tools are never registered (build errors clearly if
	// an agent's tools: list requests one anyway; see internal/serve).
	Workspace *workspace.Jail
	// WorkspaceUserID is the identity fs tools are bound to at construction
	// (the isolation model's "no identity parsing inside tool handlers" rule).
	// Quack is single-user today, so this is always the "local" constant
	// (internal/server/rest and internal/server/mcp use the same constant).
	WorkspaceUserID string
	// WorkspaceCaps bounds fs tool calls (read/write bytes, grep/glob/list
	// result caps). Zero value ⇒ workspace.DefaultCaps().
	WorkspaceCaps workspace.Caps
}

// constructor builds one tool from the shared dependencies.
type constructor func(Deps) (tool.Tool, error)

var registry = map[string]constructor{
	"web_search":   newWebSearch,
	"web_fetch":    newFetch,
	"summarize":    newSummarize,
	"current_date": newCurrentDate,
	"stage_memory": newStageMemory,
	"ask_user":     func(Deps) (tool.Tool, error) { return NewAskUserTool() },
	"ask_advisor":  func(d Deps) (tool.Tool, error) { return NewAskAdvisorTool(d.Advisor, d.Sessions) },
	// Filesystem tools (internal/tools/fs.go), all bound to (userID, jail) —
	// see fsBinding / newFSBinding.
	"read_file":   newReadFile,
	"write_file":  newWriteFile,
	"edit_file":   newEditFile,
	"list_dir":    newListDir,
	"glob":        newGlob,
	"grep":        newGrep,
	"delete_path": newDeletePath,
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

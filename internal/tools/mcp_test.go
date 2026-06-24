package tools

import (
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// TestBuildMCPToolsets_LazyAndScoped: construction is offline (no network — the
// toolsets connect lazily), and MCPToolsetsFor applies the per-server agent
// scope: an unscoped server reaches every agent, a scoped one only its list.
func TestBuildMCPToolsets_LazyAndScoped(t *testing.T) {
	sets, err := BuildMCPToolsets([]config.MCPServerConfig{
		{Name: "global", URL: "https://mcp.example.com/mcp"},
		{Name: "exa", URL: "https://mcp.exa.ai/mcp", Agents: []string{"web-researcher"}},
	})
	if err != nil {
		t.Fatalf("BuildMCPToolsets: %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("got %d toolsets, want 2", len(sets))
	}

	// web-researcher sees both (the global one + its scoped one).
	if got := len(MCPToolsetsFor(sets, "web-researcher")); got != 2 {
		t.Errorf("web-researcher sees %d toolsets, want 2", got)
	}
	// another agent sees only the unscoped server.
	if got := len(MCPToolsetsFor(sets, "synthesizer")); got != 1 {
		t.Errorf("synthesizer sees %d toolsets, want 1 (only the unscoped server)", got)
	}
}

func TestBuildMCPToolsets_RejectsEmptyURL(t *testing.T) {
	if _, err := BuildMCPToolsets([]config.MCPServerConfig{{Name: "broken"}}); err == nil {
		t.Fatal("expected error for an MCP server with no URL")
	}
}

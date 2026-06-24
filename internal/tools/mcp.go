package tools

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"

	"github.com/fagerbergj/quack/internal/config"
)

// ScopedToolset is an MCP server's toolset plus the set of agents allowed to use
// it. A nil/empty agents set means every (worker) agent gets it — matching how
// the skill toolset is shared by all.
type ScopedToolset struct {
	Name   string
	Set    tool.Toolset
	agents map[string]bool // nil/empty = all agents
}

// BuildMCPToolsets turns the configured outbound MCP servers into ADK toolsets.
// The MCP connection is lazy — ADK lists tools on first use — so this does no
// network I/O and never blocks startup on a slow/unreachable server (a bad
// endpoint surfaces when an agent first lists its tools). An optional per-server
// tool allowlist is applied via FilterToolset.
func BuildMCPToolsets(servers []config.MCPServerConfig) ([]ScopedToolset, error) {
	out := make([]ScopedToolset, 0, len(servers))
	for _, s := range servers {
		if s.URL == "" {
			return nil, fmt.Errorf("mcp server %q: url required", s.Name)
		}
		ts, err := mcptoolset.New(mcptoolset.Config{
			Transport: &mcp.StreamableClientTransport{Endpoint: s.URL},
		})
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", s.Name, err)
		}
		if len(s.Tools) > 0 {
			ts = tool.FilterToolset(ts, tool.StringPredicate(s.Tools))
		}

		scoped := ScopedToolset{Name: s.Name, Set: ts}
		if len(s.Agents) > 0 {
			scoped.agents = make(map[string]bool, len(s.Agents))
			for _, a := range s.Agents {
				scoped.agents[a] = true
			}
		}
		out = append(out, scoped)
	}
	return out, nil
}

// MCPToolsetsFor returns the toolsets visible to the named agent: every unscoped
// server plus those whose scope lists this agent.
func MCPToolsetsFor(all []ScopedToolset, agent string) []tool.Toolset {
	var ts []tool.Toolset
	for _, s := range all {
		if len(s.agents) == 0 || s.agents[agent] {
			ts = append(ts, s.Set)
		}
	}
	return ts
}

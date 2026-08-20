package plugin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// mcpSchemaID is the only mcp.json $schema quack recognizes (spec §7.2.1
// forbids fetching a schema while loading, so recognition is a literal
// match against the versions this build implements).
const mcpSchemaID = "https://agent-plugins.org/schemas/1.1.0/mcp.schema.json"

// Reserved subprocess variables (§9.1). The client supplies them; a server's
// own env may not.
const (
	envPluginRoot = "PLUGIN_ROOT"
	envPluginData = "PLUGIN_DATA"
)

// MCPServer is one stdio entry from mcp.json, still carrying its
// ${PLUGIN_ROOT}/${PLUGIN_DATA} placeholders. Only the stdio transport is
// modelled: §7.2.3 requires supporting at least one of stdio/streamable-http,
// and §7.2.2 rule 4 makes skipping an unsupported transport the conformant
// response rather than a failure.
type MCPServer struct {
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
}

// mcpFile is mcp.json's closed top-level shape.
type mcpFile struct {
	Schema     string                     `json:"$schema"`
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

// mcpEntry is one server config. The variants are closed (§7.2.1), so an
// unknown field or a field from another variant invalidates the entry -
// DisallowUnknownFields plus the per-type field checks below.
type mcpEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// loadMCP reads <root>/mcp.json per §7.2.2: an absent file is silent, a
// broken file disables MCP for this plugin only, and a bad single entry skips
// just that entry. Nothing here ever fails the run.
func loadMCP(root, name string) map[string]MCPServer {
	b, err := os.ReadFile(filepath.Join(root, "mcp.json"))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("plugin mcp.json unreadable; MCP disabled for this plugin", "component", "plugin", "plugin", name, "err", err)
		}
		return nil
	}

	var f mcpFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		slog.Warn("plugin mcp.json invalid; MCP disabled for this plugin", "component", "plugin", "plugin", name, "err", err)
		return nil
	}
	if f.Schema != mcpSchemaID {
		slog.Warn("plugin mcp.json targets an unrecognized Agent Plugins version; MCP disabled for this plugin",
			"component", "plugin", "plugin", name, "schema", f.Schema, "supported", mcpSchemaID)
		return nil
	}

	out := make(map[string]MCPServer, len(f.MCPServers))
	for server, raw := range f.MCPServers {
		s, err := parseMCPEntry(raw)
		if err != nil {
			slog.Warn("plugin mcp.json server skipped", "component", "plugin", "plugin", name, "server", server, "err", err)
			continue
		}
		if s == nil {
			continue // unsupported transport, already reported
		}
		out[server] = *s
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseMCPEntry returns nil, nil for a valid entry on a transport quack does
// not implement.
func parseMCPEntry(raw json.RawMessage) (*MCPServer, error) {
	var e mcpEntry
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return nil, err
	}
	switch e.Type {
	case "stdio":
	case "streamable-http", "sse":
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown type %q", e.Type)
	}
	if e.URL != "" || len(e.Headers) > 0 {
		return nil, fmt.Errorf("stdio entry carries remote-transport fields")
	}
	// §7.2.1: one executable token, either a bare name or a ./ plugin-relative
	// path. No shell string, no placeholder expansion.
	if e.Command == "" || strings.ContainsAny(e.Command, " \t") {
		return nil, fmt.Errorf("command %q must be a single executable token", e.Command)
	}
	if strings.Contains(e.Command, "/") && !strings.HasPrefix(e.Command, "./") {
		return nil, fmt.Errorf("command %q must be a bare name or a ./ plugin-relative path", e.Command)
	}
	for k := range e.Env {
		if k == envPluginRoot || k == envPluginData {
			return nil, fmt.Errorf("env may not set the reserved %s", k)
		}
	}
	return &MCPServer{Command: e.Command, Args: e.Args, Env: e.Env, Cwd: e.Cwd}, nil
}

// Launch expands this server's placeholders against the plugin root and its
// client-managed data directory (§9), and resolves command and cwd to
// absolute, contained paths. env is returned as KEY=VALUE overlay entries
// with the two reserved variables appended last, as §9.1 requires.
//
// cwd is always inside PLUGIN_DATA. §7.2.1 defaults it to the plugin root and
// allows ${PLUGIN_ROOT}-rooted values, but a sandboxed child's own working
// directory is necessarily writable, and a server that can rewrite its root
// can rewrite the skills/ that reach agent prompts. quack keeps the root
// read-only and diverges here on purpose - the root stays readable, so ./bin
// commands and ${PLUGIN_ROOT} references are unaffected.
func (s MCPServer) Launch(root, data string) (argv []string, env []string, cwd string, err error) {
	expand := strings.NewReplacer("${"+envPluginRoot+"}", root, "${"+envPluginData+"}", data).Replace

	command := s.Command
	if strings.HasPrefix(command, "./") {
		if command, err = containedPath(root, command); err != nil {
			return nil, nil, "", fmt.Errorf("command: %w", err)
		}
	}
	argv = append(argv, command)
	for _, a := range s.Args {
		argv = append(argv, expand(a))
	}

	for k, v := range s.Env {
		env = append(env, k+"="+expand(v))
	}
	env = append(env, envPluginRoot+"="+root, envPluginData+"="+data)

	switch {
	case s.Cwd == "":
		cwd = data
	case s.Cwd == "${"+envPluginData+"}" || strings.HasPrefix(s.Cwd, "${"+envPluginData+"}/"):
		cwd, err = containedPath(data, strings.TrimPrefix(expand(s.Cwd), data))
	default:
		err = fmt.Errorf("cwd %q must be ${%s} or a path under it", s.Cwd, envPluginData)
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("cwd: %w", err)
	}
	return argv, env, cwd, nil
}

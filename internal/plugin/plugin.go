// Package plugin discovers plugins packaged per the Agent Plugins standard
// (https://agent-plugins.org/) or its Codex predecessor. A resolved root can
// contribute three things: skills (skills/, spec §7.1), MCP servers
// (mcp.json, spec §7.2), and quack's own client-extension declarations
// (plugin.json's extensions[Namespace], spec §8). Distribution is out of
// scope - trees are vendored in-tree, see .agents/vendor/plugins.yaml.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Namespace is quack's client-extension namespace (Agent Plugins §8). It is
// derived from github.com/fagerbergj, the account controlling both quack and
// quack-extensions; quack owns no registered domain of its own.
const Namespace = "io.github.fagerbergj.quack"

// nsSchemaVersion is the only version of the namespace block quack
// understands. §8 leaves validation inside a namespace to its owner, and a
// block quack cannot read is declaring compiled-in code, so it is an error
// rather than a skip.
const nsSchemaVersion = 1

// Plugin is one resolved plugin root. Every component is optional: §6.2
// requires an absent fixed location to be a non-error, so a plugin may carry
// only skills, only MCP servers, only module declarations, or any mix.
type Plugin struct {
	Name string
	Root string

	// SkillsDir is the absolute skills directory, or "" when the plugin
	// ships none.
	SkillsDir string

	// Modules are the compiled-in Go modules this plugin declares. quack
	// cannot load Go code dynamically, so these are checked against the
	// linked registry at boot, never loaded.
	Modules []Module

	// ConfigRequired mirrors the namespace block's "config": a module-bearing
	// plugin keeps fail-on-empty-config boot semantics, a skill-only one
	// warns and skips.
	ConfigRequired bool

	// MCPServers are mcp.json's stdio entries, unexpanded - ${PLUGIN_DATA}
	// is only known to the caller that owns the data directory.
	MCPServers map[string]MCPServer
}

// Module is one host-coupled Go module a plugin declares. Path is carried so
// a boot failure can name the import a developer has to add; quack never
// resolves it at runtime.
type Module struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// rootManifest is the Agent Plugins root plugin.json. The portable schema is
// closed (additionalProperties: false) with only $schema and name required,
// so extensions is the only place client-specific data can legally live.
type rootManifest struct {
	Name       string                     `json:"name"`
	Extensions map[string]json.RawMessage `json:"extensions"`
}

// nsBlock is extensions["io.github.fagerbergj.quack"].
type nsBlock struct {
	SchemaVersion int      `json:"schemaVersion"`
	Modules       []Module `json:"modules"`
	Config        string   `json:"config"`
}

// codexManifest is .codex-plugin/plugin.json: the same identity fields as the
// Agent Plugins manifest, plus an explicit "skills" path. Its "interface"
// block (Codex UI metadata) is deliberately not decoded - never read, never a
// reason to fail.
type codexManifest struct {
	Name   string `json:"name"`
	Skills string `json:"skills"`
}

// NamespaceError is a malformed extensions[Namespace] block - the one plugin
// failure quack refuses to downgrade to a warning.
type NamespaceError struct {
	Root string
	Err  error
}

func (e *NamespaceError) Error() string {
	return fmt.Sprintf("plugin %s: extensions[%q]: %v", e.Root, Namespace, e.Err)
}

func (e *NamespaceError) Unwrap() error { return e.Err }

// Resolve resolves each root, in order, via: root plugin.json (Agent Plugins)
// else .codex-plugin/plugin.json (Codex) else skipped. Detection is by path,
// never by sniffing fields inside either file. A root that fails to resolve
// logs a warning naming it and the reason, then is dropped - a broken or
// missing plugin never fails the run, it just loses that plugin's components.
//
// The one exception is quack's own namespace block: an unreadable one is
// returned as an error, because it declares compiled-in Go code and silently
// dropping it would boot a server missing the module it promised.
func Resolve(roots []string) ([]Plugin, error) {
	var out []Plugin
	for _, root := range roots {
		p, err := resolveRoot(root)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, nil
}

// ResolveSkillDirs returns just the skills directories of the resolved roots,
// in order and never deduped: callers control precedence via root order. A
// namespace error is ignored here - the skills path stays warn-and-skip, and
// boot surfaces the same error through Resolve.
func ResolveSkillDirs(roots []string) []string {
	plugins, _ := Resolve(roots)
	var dirs []string
	for _, p := range plugins {
		if p.SkillsDir != "" {
			dirs = append(dirs, p.SkillsDir)
		}
	}
	return dirs
}

// resolveRoot returns nil, nil for a root that is skipped.
func resolveRoot(root string) (*Plugin, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		slog.Warn("plugin root path invalid; skipped", "component", "plugin", "root", root, "err", err)
		return nil, nil
	}

	p, err := fromRootManifest(abs)
	if err == nil {
		p.MCPServers = loadMCP(abs, p.Name)
		return p, nil
	}
	var nsErr *NamespaceError
	if errors.As(err, &nsErr) {
		return nil, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("plugin.json invalid; skipped", "component", "plugin", "root", abs, "err", err)
		return nil, nil
	}

	p, err = fromCodexManifest(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("no plugin.json or .codex-plugin/plugin.json found; skipped", "component", "plugin", "root", abs)
		} else {
			slog.Warn(".codex-plugin/plugin.json invalid; skipped", "component", "plugin", "root", abs, "err", err)
		}
		return nil, nil
	}
	return p, nil
}

// fromRootManifest reads <root>/plugin.json. A missing file returns a wrapped
// os.ErrNotExist so the caller falls through to the Codex format; any other
// failure is terminal for this root - a manifest that exists but is broken is
// never reinterpreted as merely absent.
func fromRootManifest(abs string) (*Plugin, error) {
	b, err := os.ReadFile(filepath.Join(abs, "plugin.json"))
	if err != nil {
		return nil, err
	}
	var m rootManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse plugin.json: %w", err)
	}

	p := &Plugin{Name: m.Name, Root: abs}
	// §6.2: an absent skills/ is not an error - a plugin may carry only MCP
	// servers or only module declarations.
	dir := filepath.Join(abs, "skills")
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		p.SkillsDir = dir
	}
	// §8: namespaces quack does not implement are ignored WITHOUT validating
	// their contents - only our own key is ever decoded.
	if raw, ok := m.Extensions[Namespace]; ok {
		if err := applyNamespace(p, raw); err != nil {
			return nil, &NamespaceError{Root: abs, Err: err}
		}
	}
	slog.Info("plugin resolved", "component", "plugin", "format", "agent-plugins", "root", abs, "name", m.Name,
		"skills", p.SkillsDir != "", "modules", len(p.Modules))
	return p, nil
}

func applyNamespace(p *Plugin, raw json.RawMessage) error {
	var ns nsBlock
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ns); err != nil {
		return err
	}
	if ns.SchemaVersion != nsSchemaVersion {
		return fmt.Errorf("schemaVersion %d unsupported (this quack understands %d)", ns.SchemaVersion, nsSchemaVersion)
	}
	for _, mod := range ns.Modules {
		if mod.Name == "" || mod.Path == "" {
			return fmt.Errorf("modules entry needs both \"name\" and \"path\", got %+v", mod)
		}
	}
	switch ns.Config {
	case "", "optional":
	case "required":
		p.ConfigRequired = true
	default:
		return fmt.Errorf("config %q is not \"required\" or \"optional\"", ns.Config)
	}
	p.Modules = ns.Modules
	return nil
}

// fromCodexManifest reads <root>/.codex-plugin/plugin.json. Its "skills"
// value is plugin-relative and resolved under abs; a value that escapes the
// root is refused (error, not silently followed). The Codex format predates
// the extensions field, so it never carries a namespace block.
func fromCodexManifest(abs string) (*Plugin, error) {
	b, err := os.ReadFile(filepath.Join(abs, ".codex-plugin", "plugin.json"))
	if err != nil {
		return nil, err
	}
	var m codexManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse .codex-plugin/plugin.json: %w", err)
	}
	if strings.TrimSpace(m.Skills) == "" {
		return nil, fmt.Errorf("plugin %q has no \"skills\" field", m.Name)
	}
	dir, err := containedPath(abs, filepath.FromSlash(m.Skills))
	if err != nil {
		return nil, fmt.Errorf("plugin %q skills path %q escapes the plugin root", m.Name, m.Skills)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("plugin %q skills directory %q does not exist", m.Name, m.Skills)
	}
	slog.Info("plugin resolved", "component", "plugin", "format", "codex", "root", abs, "name", m.Name)
	return &Plugin{Name: m.Name, Root: abs, SkillsDir: dir}, nil
}

// containedPath joins rel under base and refuses any result that escapes it.
func containedPath(base, rel string) (string, error) {
	p := filepath.Clean(filepath.Join(base, rel))
	r, err := filepath.Rel(base, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes %q", rel, base)
	}
	return p, nil
}

// Package plugin discovers plugins packaged per the Agent Plugins standard (https://agent-plugins.org/) or its Codex predecessor. A resolved root can
// contribute skills (skills/, spec §7.1), MCP servers (mcp.json, spec §7.2), quack's own client-extension declarations (plugin.json's extensions[Namespace], spec §8), and - Agent Plugins format only - agent bundles (agents/) and workflow shapes (workflows/), quack's own layout additions.
// Distribution is out of scope - see internal/pluginreg for the fetch/clone side.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Namespace is quack's client-extension namespace (Agent Plugins §8). It is
// derived from github.com/fagerbergj, the account controlling both quack and
// quack-extensions; quack owns no registered domain of its own.
const Namespace = "io.github.fagerbergj.quack"

// nsSchemaVersion is the only version of the namespace block quack
// understands. §8 leaves validation inside a namespace to its owner, and a
// block quack cannot read is declaring compiled-in code, so it is an error rather than a skip.
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

	// AgentsDir is the absolute agents/ directory (agent-card.json/prompt.md
	// bundles, one subdirectory per bundle), or "" when the plugin ships none.
	AgentsDir string

	// WorkflowsDir is the absolute workflows/ directory (one *.yaml shape per
	// file, config.WorkflowShape's own schema), or "" when the plugin ships none.
	WorkflowsDir string

	// Agents is the namespace block's "agents" list - the seeding contract.
	// Nil (list omitted, or no namespace block at all) means nothing seeds from AgentsDir.
	Agents []string

	// Workflows is the namespace block's "workflows" list, same contract as Agents.
	Workflows []string

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
	// Agents/Workflows: the only input to seeding - nil (key omitted) means
	// nothing seeds, same as an explicit empty list.
	Agents    []string `json:"agents"`
	Workflows []string `json:"workflows"`
}

// codexManifest is .codex-plugin/plugin.json: the same identity fields as the
// Agent Plugins manifest, plus an explicit "skills" path. Its "interface"
// block (Codex UI metadata) is deliberately not decoded - never read, never a reason to fail.
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
// returned as an error, because it declares compiled-in Go code and silently dropping it would boot a server missing the module it promised.
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
// failure is terminal for this root - a manifest that exists but is broken is never reinterpreted as merely absent.
func fromRootManifest(abs string) (*Plugin, error) {
	b, err := os.ReadFile(filepath.Join(abs, "plugin.json"))
	if err != nil {
		return nil, err
	}
	var m rootManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse plugin.json: %w", err)
	}
	if strings.TrimSpace(m.Name) == "" {
		return nil, fmt.Errorf("plugin.json at %s: \"name\" is required", abs)
	}

	p := &Plugin{Name: m.Name, Root: abs}
	// §6.2: an absent skills/ is not an error - a plugin may carry only MCP
	// servers or module declarations. agents/ and workflows/ are discovered the same way, gated by the namespace block's lists below.
	dir := filepath.Join(abs, "skills")
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		p.SkillsDir = dir
	}
	if dir := filepath.Join(abs, "agents"); dirExists(dir) {
		p.AgentsDir = dir
	}
	if dir := filepath.Join(abs, "workflows"); dirExists(dir) {
		p.WorkflowsDir = dir
	}
	// §8: namespaces quack does not implement are ignored WITHOUT validating
	// their contents - only our own key is ever decoded.
	if raw, ok := m.Extensions[Namespace]; ok {
		if err := applyNamespace(p, raw); err != nil {
			return nil, &NamespaceError{Root: abs, Err: err}
		}
	}
	slog.Info("plugin resolved", "component", "plugin", "format", "agent-plugins", "root", abs, "name", m.Name,
		"skills", p.SkillsDir != "", "agents", p.AgentsDir != "", "workflows", p.WorkflowsDir != "", "modules", len(p.Modules))
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
	p.Agents = ns.Agents
	p.Workflows = ns.Workflows
	return nil
}

// CheckManifestLists fails (NamespaceError-class, naming the entry) when a
// listed agent/workflow isn't actually present - called from internal/serve's admission path, not Resolve.
func CheckManifestLists(p Plugin) (err error) {
	agentsPresent := dirEntryNames(p.AgentsDir)
	workflowsPresent, err := yamlEntryNames(p.WorkflowsDir)
	if err != nil {
		return &NamespaceError{Root: p.Root, Err: err}
	}
	if err := checkListedPresent("agents", p.Agents, agentsPresent); err != nil {
		return &NamespaceError{Root: p.Root, Err: err}
	}
	if err := checkListedPresent("workflows", p.Workflows, workflowsPresent); err != nil {
		return &NamespaceError{Root: p.Root, Err: err}
	}
	return nil
}

// WarnUnlistedManifestEntries logs one warning per present-but-unlisted
// entry - fired even with no namespace block at all, which means an empty list, not "everything".
func WarnUnlistedManifestEntries(p Plugin) {
	warnUnlisted(p.Name, "agents", p.Agents, dirEntryNames(p.AgentsDir))
	workflowsPresent, err := yamlEntryNames(p.WorkflowsDir)
	if err != nil {
		return // malformed shape - CheckManifestLists reports it, not this diagnostic pass
	}
	warnUnlisted(p.Name, "workflows", p.Workflows, workflowsPresent)
}

// checkListedPresent fails when a listed name isn't actually present.
func checkListedPresent(kind string, listed []string, present map[string]bool) error {
	for _, name := range listed {
		if !present[name] {
			return fmt.Errorf("%s entry %q not found in plugin", kind, name)
		}
	}
	return nil
}

// warnUnlisted logs one warning per present name absent from listed.
func warnUnlisted(pluginName, kind string, listed []string, present map[string]bool) {
	listedSet := make(map[string]bool, len(listed))
	for _, name := range listed {
		listedSet[name] = true
	}
	for name := range present {
		if !listedSet[name] {
			slog.Warn("plugin bundle present but not listed in manifest; not seeded",
				"component", "plugin", "plugin", pluginName, "kind", kind, "name", name)
		}
	}
}

// dirEntryNames lists agents/<name>/ names that are actually bundles (carry
// agent-card.json) - the same predicate config.SeedPluginAgents seeds by, so
// "present" here can never diverge from what would actually seed.
func dirEntryNames(dir string) map[string]bool {
	out := map[string]bool{}
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "agent-card.json")); err != nil {
			continue
		}
		out[e.Name()] = true
	}
	return out
}

// yamlEntryNames lists workflows/*.yaml file stems whose internal name:
// field matches the stem - the identity every other check keys on; a mismatch errors rather than being silently admitted.
func yamlEntryNames(dir string) (map[string]bool, error) {
	out := map[string]bool{}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, nil
	}
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		name, err := workflowShapeName(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("workflows/%s: %w", e.Name(), err)
		}
		if name != stem {
			return nil, fmt.Errorf("workflows/%s: name %q does not match its filename", e.Name(), name)
		}
		out[stem] = true
	}
	return out, nil
}

// workflowShapeName reads only the "name" field of a workflow shape yaml -
// config.WorkflowShape owns the full schema, this just needs identity.
func workflowShapeName(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var shape struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(raw, &shape); err != nil {
		return "", err
	}
	return shape.Name, nil
}

// fromCodexManifest reads <root>/.codex-plugin/plugin.json. Its "skills"
// value is plugin-relative and resolved under abs; a value that escapes the
// root is refused (error, not silently followed). The Codex format predates the extensions field, so it never carries a namespace block.
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

func dirExists(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
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

// SkillDirs is the skills directories of already-resolved plugins, in order
// and never deduped. Callers that resolved once at boot use this instead of
// ResolveSkillDirs, which would re-read every manifest.
func SkillDirs(plugins []Plugin) []string {
	var dirs []string
	for _, p := range plugins {
		if p.SkillsDir != "" {
			dirs = append(dirs, p.SkillsDir)
		}
	}
	return dirs
}

// Package plugin resolves plugin roots in the Agent Plugins (https://agent-plugins.org/) or Codex layout into
// skills, MCP servers, namespace declarations, agent bundles and workflows; fetching lives in internal/pluginreg.
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

// Namespace is quack's client-extension namespace (Agent Plugins §8), derived from the GitHub account that
// owns quack, since quack has no registered domain.
const Namespace = "io.github.fagerbergj.quack"

// nsSchemaVersion is the only namespace block version quack reads. A block it cannot read declares
// compiled-in code, so it is an error rather than a skip.
const nsSchemaVersion = 1

// Plugin is one resolved plugin root. Every component is optional (§6.2: an absent fixed location is
// not an error).
type Plugin struct {
	Name string
	Root string

	// SHA is the registry row's fetched commit, stamped by the host; "" for a
	// local row, whose Root then stands in for its revision.
	SHA string

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

	// Modules are compiled-in Go modules this plugin declares; quack cannot load Go dynamically, so they are
	// only checked against the linked registry at boot.
	Modules []Module

	// ConfigRequired mirrors the namespace block's "config": a module-bearing plugin fails boot on empty
	// config, a skill-only one warns and skips.
	ConfigRequired bool

	// MCPServers are mcp.json's stdio entries, unexpanded - ${PLUGIN_DATA}
	// is only known to the caller that owns the data directory.
	MCPServers map[string]MCPServer
	// MCPSkipped is why mcp.json entries were left out, by server ("" = the whole file).
	MCPSkipped map[string]string
}

// Module is one Go module a plugin declares. Path lets a boot failure name the import to add.
type Module struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// rootManifest is the Agent Plugins root plugin.json. Its schema is closed, so extensions is the only
// legal home for client-specific data.
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

// codexManifest is .codex-plugin/plugin.json; its "interface" block (Codex UI metadata) is deliberately
// not decoded, so it can never fail a load.
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

// Resolve resolves each root via plugin.json, else .codex-plugin/plugin.json, else skips it with a warning.
// An unreadable quack namespace block is an error: dropping it would boot without a promised module.
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
		p.MCPServers, p.MCPSkipped = loadMCP(abs, p.Name)
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

// fromRootManifest returns a wrapped os.ErrNotExist for a missing file so the caller tries Codex; a
// manifest that exists but is broken is terminal for this root.
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
	// §6.2: an absent skills/ is not an error; agents/ and workflows/ are found the same way, gated by the
	// namespace block's lists.
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
	if err := checkNoDuplicates("agents", p.Agents); err != nil {
		return &NamespaceError{Root: p.Root, Err: err}
	}
	if err := checkNoDuplicates("workflows", p.Workflows); err != nil {
		return &NamespaceError{Root: p.Root, Err: err}
	}
	agentsPresent := dirEntryNames(p.AgentsDir)
	workflowsPresent, err := yamlEntryNames(p.Name, p.WorkflowsDir, p.Workflows, false)
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
	// A listed shape's own parse/mismatch failure is CheckManifestLists' to
	// report; this is the one pass that warns about the unlisted ones.
	workflowsPresent, _ := yamlEntryNames(p.Name, p.WorkflowsDir, p.Workflows, true)
	warnUnlisted(p.Name, "workflows", p.Workflows, workflowsPresent)
}

// checkNoDuplicates fails on a name listed twice in one plugin's own list -
// a manifest error on its own, so it is refused whether or not the module gate is on.
func checkNoDuplicates(kind string, listed []string) error {
	seen := make(map[string]bool, len(listed))
	for _, name := range listed {
		if seen[name] {
			return fmt.Errorf("%s entry %q listed twice", kind, name)
		}
		seen[name] = true
	}
	return nil
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
			slog.Warn("plugin entry present but not listed in manifest; not seeded",
				"component", "plugin", "plugin", pluginName, "kind", kind, "name", name)
		}
	}
}

// dirEntryNames lists agents/<name>/ dirs carrying agent-card.json, the same predicate
// config.SeedPluginAgents seeds by.
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

// yamlEntryNames lists workflows/*.yaml stems whose internal name: matches
// the stem. A LISTED entry that fails to parse or mismatch errors; an unlisted one only warns and is excluded.
func yamlEntryNames(pluginName, dir string, listed []string, warnInvalid bool) (map[string]bool, error) {
	out := map[string]bool{}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, nil
	}
	listedSet := make(map[string]bool, len(listed))
	for _, n := range listed {
		listedSet[n] = true
	}
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		name, err := workflowShapeName(filepath.Join(dir, e.Name()))
		if err == nil && name == stem {
			out[stem] = true
			continue
		}
		if listedSet[stem] {
			if err != nil {
				return out, fmt.Errorf("workflows/%s: %w", e.Name(), err)
			}
			return out, fmt.Errorf("workflows/%s: name %q does not match its filename", e.Name(), name)
		}
		if warnInvalid {
			slog.Warn("plugin workflow shape invalid; skipped, not seeded",
				"component", "plugin", "plugin", pluginName, "file", e.Name())
		}
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

// fromCodexManifest resolves "skills" under abs and refuses a path escaping the root. Codex has no
// extensions field, so no namespace block.
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

// SkillDirs is the skills directories of already-resolved plugins, in order and not deduped; it avoids
// re-reading manifests like ResolveSkillDirs does.
func SkillDirs(plugins []Plugin) []string {
	var dirs []string
	for _, p := range plugins {
		if p.SkillsDir != "" {
			dirs = append(dirs, p.SkillsDir)
		}
	}
	return dirs
}

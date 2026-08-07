// Package plugin discovers skill libraries packaged per the Agent Plugins
// standard (https://agent-plugins.org/) or its Codex predecessor, so quack
// reads a vendored plugin's layout instead of hardcoding it. Distribution
// stays whatever the caller already uses (a git submodule today) - this
// package only resolves an on-disk root to its skills directory.
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

// rootManifest is the Agent Plugins root plugin.json. Its schema requires only
// $schema and name and forbids additional properties, so there is no skills
// field to read - this only confirms the directory IS a plugin and names it;
// skills live at the conventional <root>/skills.
type rootManifest struct {
	Name string `json:"name"`
}

// codexManifest is .codex-plugin/plugin.json: the same identity fields as the
// Agent Plugins manifest, plus an explicit "skills" path. Its "interface"
// block (Codex UI metadata) is deliberately not decoded - never read, never a
// reason to fail.
type codexManifest struct {
	Name   string `json:"name"`
	Skills string `json:"skills"`
}

// ResolveSkillDirs resolves each configured plugin root to its skills
// directory, in order, via: root plugin.json (Agent Plugins - skills/ by
// convention) else .codex-plugin/plugin.json (Codex - explicit "skills"
// field) else skipped. Detection is by path, never by sniffing fields inside
// either file. A root that fails at any step logs a warning naming it and the
// reason, then is dropped - a broken or missing plugin never fails the run,
// it just loses that plugin's skills. Order is preserved and never deduped:
// callers control precedence via root order.
func ResolveSkillDirs(roots []string) []string {
	var dirs []string
	for _, root := range roots {
		if dir, ok := resolveRoot(root); ok {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func resolveRoot(root string) (string, bool) {
	abs, err := filepath.Abs(root)
	if err != nil {
		slog.Warn("plugin root path invalid; skipped", "component", "plugin", "root", root, "err", err)
		return "", false
	}

	dir, err := skillsFromRootManifest(abs)
	if err == nil {
		return dir, true
	}
	if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("plugin.json invalid; skipped", "component", "plugin", "root", abs, "err", err)
		return "", false
	}

	dir, err = skillsFromCodexManifest(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("no plugin.json or .codex-plugin/plugin.json found; skipped", "component", "plugin", "root", abs)
		} else {
			slog.Warn(".codex-plugin/plugin.json invalid; skipped", "component", "plugin", "root", abs, "err", err)
		}
		return "", false
	}
	return dir, true
}

// skillsFromRootManifest reads <root>/plugin.json. A missing file returns a
// wrapped os.ErrNotExist so the caller falls through to the Codex format; any
// other failure (unreadable, malformed JSON, no skills/ dir) is terminal for
// this root - a manifest that exists but is broken is never reinterpreted as
// merely absent.
func skillsFromRootManifest(abs string) (string, error) {
	b, err := os.ReadFile(filepath.Join(abs, "plugin.json"))
	if err != nil {
		return "", err
	}
	var m rootManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("parse plugin.json: %w", err)
	}
	dir := filepath.Join(abs, "skills")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("plugin %q has no skills/ directory", m.Name)
	}
	slog.Info("plugin resolved", "component", "plugin", "format", "agent-plugins", "root", abs, "name", m.Name)
	return dir, nil
}

// skillsFromCodexManifest reads <root>/.codex-plugin/plugin.json. Its
// "skills" value is plugin-relative and resolved under abs; a value that
// escapes the root is refused (error, not silently followed).
func skillsFromCodexManifest(abs string) (string, error) {
	b, err := os.ReadFile(filepath.Join(abs, ".codex-plugin", "plugin.json"))
	if err != nil {
		return "", err
	}
	var m codexManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("parse .codex-plugin/plugin.json: %w", err)
	}
	if strings.TrimSpace(m.Skills) == "" {
		return "", fmt.Errorf("plugin %q has no \"skills\" field", m.Name)
	}
	dir := filepath.Clean(filepath.Join(abs, filepath.FromSlash(m.Skills)))
	if rel, err := filepath.Rel(abs, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("plugin %q skills path %q escapes the plugin root", m.Name, m.Skills)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("plugin %q skills directory %q does not exist", m.Name, m.Skills)
	}
	slog.Info("plugin resolved", "component", "plugin", "format", "codex", "root", abs, "name", m.Name)
	return dir, nil
}

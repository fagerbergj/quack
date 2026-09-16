package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"testing/fstest"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/skillsource"
)

// ErrPinned marks a REST plugin-registry mutation refused because a replay
// bundle has this process's skill roster pinned (#1427 P4 review F1) - rest
// maps it to 409, not the generic 500/422.
var ErrPinned = errors.New("plugin roster is pinned to a replay bundle")

// NewSkillSource builds the replay-pinned plugin skill source: a recorded
// sha is served from the clone's own history; a no-sha row is scoped out of live instead.
func NewSkillSource(ctx context.Context, sess *Session, registryRoot string, rows []pluginreg.Plugin, live skill.Source) (skill.Source, error) {
	recorded, err := sess.Plugins()
	if err != nil {
		return nil, err
	}
	if len(recorded) == 0 {
		return nil, nil
	}

	var sources []skill.Source
	seen := make(map[string]bool, len(recorded))
	for _, row := range rows {
		sha, ok := recorded[row.Name]
		if !ok {
			continue
		}
		seen[row.Name] = true
		if sha == "" {
			names, err := prefixedNames(ctx, live, row.Name)
			if err != nil {
				return nil, fmt.Errorf("replay: plugin %q: %w", row.Name, err)
			}
			if len(names) == 0 {
				// quack is always in scope (#1427 P1) - zero live skills for
				// it is a real gap; any other plugin just has none to scope.
				if row.Name == pluginreg.EmbeddedQuackPluginName {
					return nil, fmt.Errorf("replay: plugin %q: recorded with no sha, but the live roster serves none of its skills", row.Name)
				}
				slog.Warn("replay: recorded plugin has no sha and no live skills; skipping", "component", "replay", "plugin", row.Name)
				continue
			}
			sources = append(sources, skillsource.Scoped(live, names))
			continue
		}
		skillsDir, err := manifestSkillsDir(ctx, registryRoot, row.Name, sha, row.Path)
		if err != nil {
			// A missing clone or unknown sha refuses here, naming the plugin and sha.
			return nil, fmt.Errorf("replay: %w", err)
		}
		if skillsDir == "" {
			slog.Warn("replay: recorded plugin has no manifest at its recorded sha; skipping its skills", "component", "replay", "plugin", row.Name, "sha", sha)
			continue
		}
		treeFS, err := pluginreg.TreeAt(ctx, registryRoot, row.Name, sha, path.Join(row.Path, skillsDir))
		if err != nil {
			return nil, fmt.Errorf("replay: %w", err)
		}
		sources = append(sources, skillsource.Prefixed(row.Name, skillsource.Tolerant(skillsource.NewFileSystemSource(treeFS), treeFS, row.Name+"@"+sha)))
	}
	for name := range recorded {
		if !seen[name] {
			return nil, fmt.Errorf("replay: plugin %q: not in the registry, cannot locate its clone", name)
		}
	}
	return skill.NewMergedSource(sources...), nil
}

// manifestSkillsDir resolves a plugin's skills directory at a recorded sha,
// mirroring plugin.Resolve's live detection order; "" means neither admits.
func manifestSkillsDir(ctx context.Context, registryRoot, name, sha, pluginPath string) (string, error) {
	root, err := pluginreg.TreeAt(ctx, registryRoot, name, sha, path.Join(pluginPath, "plugin.json"))
	if err != nil {
		return "", err
	}
	if data, ok := soleFile(root); ok {
		var m struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &m) == nil && strings.TrimSpace(m.Name) != "" {
			return "skills", nil
		}
		return "", nil // invalid manifest never falls through to codex, mirroring fromRootManifest
	}
	codex, err := pluginreg.TreeAt(ctx, registryRoot, name, sha, path.Join(pluginPath, ".codex-plugin/plugin.json"))
	if err != nil {
		return "", err
	}
	data, ok := soleFile(codex)
	if !ok {
		return "", nil
	}
	var m struct {
		Skills string `json:"skills"`
	}
	if json.Unmarshal(data, &m) != nil || strings.TrimSpace(m.Skills) == "" {
		return "", nil
	}
	return m.Skills, nil
}

// soleFile returns the one file TreeAt's single-path pathspec matched, if any.
func soleFile(tree fstest.MapFS) ([]byte, bool) {
	for _, f := range tree {
		return f.Data, true
	}
	return nil, false
}

// prefixedNames lists live's skill names qualified "plugin:skill" under
// plugin - the live roster's own naming rule (skillsource.Prefixed), reused
// here to scope a sha-less recorded plugin to just its own skills.
func prefixedNames(ctx context.Context, live skill.Source, plugin string) ([]string, error) {
	fms, err := live.ListFrontmatters(ctx)
	if err != nil {
		return nil, err
	}
	prefix := plugin + ":"
	var names []string
	for _, fm := range fms {
		if strings.HasPrefix(fm.Name, prefix) {
			names = append(names, fm.Name)
		}
	}
	return names, nil
}

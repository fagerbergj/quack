package replay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing/fstest"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/skillsource"
)

// ErrPinned marks a REST plugin-registry mutation refused because a replay
// bundle has this process's skill roster pinned (#1427 P4 review F1) - rest
// maps it to 409, not the generic 500/422.
var ErrPinned = errors.New("plugin roster is pinned to a replay bundle")

// NewSkillSource builds the replay-pinned plugin skill source (#1427 P4): a
// recorded sha is served via git show against its own clone history, never
// live admission (admitted is unused here); a no-sha row is scoped out of
// live instead, keeping live's own embedded/backfill rules. rows' order
// drives merge order (deterministic bare-name resolve, F3).
func NewSkillSource(ctx context.Context, sess *Session, registryRoot string, rows []pluginreg.Plugin, admitted []plugin.Plugin, live skill.Source) (skill.Source, error) {
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
		// One TreeAt call covers both the manifest check and the skills
		// tree; a missing clone or unknown sha refuses here, naming both.
		tree, err := pluginreg.TreeAt(ctx, registryRoot, row.Name, sha, row.Path)
		if err != nil {
			return nil, fmt.Errorf("replay: %w", err)
		}
		if !hasManifest(tree) {
			// Mirrors admitPlugins' live rule, decided from the recorded
			// sha's tree instead of the clone's current disk state.
			slog.Warn("replay: recorded plugin has no manifest at its recorded sha; skipping its skills", "component", "replay", "plugin", row.Name, "sha", sha)
			continue
		}
		treeFS := skillsSubtree(tree)
		sources = append(sources, skillsource.Prefixed(row.Name, skillsource.Tolerant(skillsource.NewFileSystemSource(treeFS), treeFS, row.Name+"@"+sha)))
	}
	for name := range recorded {
		if !seen[name] {
			return nil, fmt.Errorf("replay: plugin %q: not in the registry, cannot locate its clone", name)
		}
	}
	return skill.NewMergedSource(sources...), nil
}

// hasManifest reports whether tree carries either manifest path
// plugin.Resolve detects, checked at a recorded sha instead of on disk.
func hasManifest(tree fstest.MapFS) bool {
	_, agentPlugins := tree["plugin.json"]
	_, codex := tree[".codex-plugin/plugin.json"]
	return agentPlugins || codex
}

// skillsSubtree narrows a plugin root tree to its skills/ prefix, the shape
// skillsource.NewFileSystemSource expects.
func skillsSubtree(tree fstest.MapFS) fstest.MapFS {
	out := make(fstest.MapFS, len(tree))
	for p, f := range tree {
		if rel, ok := strings.CutPrefix(p, "skills/"); ok {
			out[rel] = f
		}
	}
	return out
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

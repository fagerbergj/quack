package replay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

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
// recorded sha is served via git show; a no-sha row is scoped out of live
// instead, keeping live's own embedded/backfill rules. rows' order drives merge order (deterministic bare-name resolve, F3).
func NewSkillSource(ctx context.Context, sess *Session, registryRoot string, rows []pluginreg.Plugin, admitted []plugin.Plugin, live skill.Source) (skill.Source, error) {
	recorded, err := sess.Plugins()
	if err != nil {
		return nil, err
	}
	if len(recorded) == 0 {
		return nil, nil
	}
	admittedNames := make(map[string]bool, len(admitted))
	for _, p := range admitted {
		admittedNames[p.Name] = true
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
				return nil, fmt.Errorf("replay: plugin %q: recorded with no sha, but the live roster serves none of its skills", row.Name)
			}
			sources = append(sources, skillsource.Scoped(live, names))
			continue
		}
		if !admittedNames[row.Name] {
			// Live skips a row with no plugin.json (admitPlugins never
			// admitted it) - replay must not serve it either (#1427 F4).
			slog.Warn("replay: recorded plugin was not admitted live; skipping its skills", "component", "replay", "plugin", row.Name)
			continue
		}
		treeFS, err := pluginreg.TreeAt(ctx, registryRoot, row.Name, sha, path.Join(row.Path, "skills"))
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

package replay

import (
	"context"
	"fmt"
	"os"
	"path"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/skillsource"
)

// NewSkillSource builds the plugin skill source for a replay run (#1427 P4):
// every plugin sess's agent.invoke entries recorded a sha for is served from
// that sha's clone content via git show, prefixed "plugin:skill" exactly
// like the live source (internal/serve.resolvedSkillSource); a recorded row
// with no sha (embedded/local, which have no clone to pin) falls through to
// its live entry in current. Resolves and verifies every sha eagerly, so an
// unreproducible plugin fails replay's setup, not a round mid-run.
//
// (nil, nil) when the bundle recorded no plugin provenance at all (a
// workflow with no ACP round - #1427 P1 only stamps plugins on agent.invoke,
// native rounds carry none): the caller keeps serving the live source.
func NewSkillSource(ctx context.Context, sess *Session, registryRoot string, rows []pluginreg.Plugin, current []plugin.Plugin) (skill.Source, error) {
	recorded, err := sess.Plugins()
	if err != nil {
		return nil, err
	}
	if len(recorded) == 0 {
		return nil, nil
	}
	rowByName := make(map[string]pluginreg.Plugin, len(rows))
	for _, r := range rows {
		rowByName[r.Name] = r
	}
	liveByName := make(map[string]plugin.Plugin, len(current))
	for _, p := range current {
		liveByName[p.Name] = p
	}

	var sources []skill.Source
	for name, sha := range recorded {
		if sha == "" {
			if p, ok := liveByName[name]; ok && p.SkillsDir != "" {
				dirFS := os.DirFS(p.SkillsDir)
				sources = append(sources, skillsource.Prefixed(name, skillsource.Tolerant(skill.NewFileSystemSource(dirFS), dirFS, p.SkillsDir)))
			}
			continue
		}
		row, ok := rowByName[name]
		if !ok {
			return nil, fmt.Errorf("replay: plugin %q@%s: not in the registry, cannot locate its clone", name, sha)
		}
		if err := pluginreg.VerifyCommit(ctx, registryRoot, name, sha); err != nil {
			return nil, fmt.Errorf("replay: %w", err)
		}
		treeFS, err := pluginreg.TreeAt(ctx, registryRoot, name, sha, path.Join(row.Path, "skills"))
		if err != nil {
			return nil, fmt.Errorf("replay: %w", err)
		}
		sources = append(sources, skillsource.Prefixed(name, skillsource.Tolerant(skill.NewFileSystemSource(treeFS), treeFS, name+"@"+sha)))
	}
	return skill.NewMergedSource(sources...), nil
}

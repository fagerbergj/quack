package skillsource

import (
	"context"
	"errors"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// resolveName: a literal match wins, else the first skill in src's order whose bare name equals name
// (merge-order "first plugin wins").
func resolveName(ctx context.Context, src skill.Source, name string) (string, error) {
	if _, err := src.LoadFrontmatter(ctx, name); err == nil {
		return name, nil
	} else if !errors.Is(err, skill.ErrSkillNotFound) {
		return "", err
	}
	fms, err := src.ListFrontmatters(ctx)
	if err != nil {
		return "", err
	}
	for _, fm := range fms {
		if BareName(fm.Name) == name {
			return fm.Name, nil
		}
	}
	return "", skill.ErrSkillNotFound
}

// Resolve loads name's frontmatter with the same bare-name fallback Scoped gives agents, for boot-time
// lookups against a plugin-qualified library.
func Resolve(ctx context.Context, src skill.Source, name string) (*skill.Frontmatter, error) {
	qualified, err := resolveName(ctx, src, name)
	if err != nil {
		return nil, err
	}
	return src.LoadFrontmatter(ctx, qualified)
}

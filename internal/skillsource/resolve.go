package skillsource

import (
	"context"
	"errors"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// ResolveName resolves name against src: a literal match wins outright,
// else the FIRST skill in src's list order whose bare name equals name wins -
// the merge-order "first plugin wins" rule every bare-name lookup here uses.
func ResolveName(ctx context.Context, src skill.Source, name string) (string, error) {
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

// Resolve loads name's frontmatter via ResolveName - the same bare-name
// fallback Scoped gives agents, for the boot-time lookups that must succeed
// against a plugin-qualified library (#1427 S1).
func Resolve(ctx context.Context, src skill.Source, name string) (*skill.Frontmatter, error) {
	qualified, err := ResolveName(ctx, src, name)
	if err != nil {
		return nil, err
	}
	return src.LoadFrontmatter(ctx, qualified)
}

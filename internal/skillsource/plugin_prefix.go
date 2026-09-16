package skillsource

import (
	"context"
	"io"
	"strings"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// Prefixed qualifies every skill name from src as "plugin:name" (#1427's
// plugin:skill naming) - the only thing that lets two plugins ship a
// same-named skill through skill.NewMergedSource without ErrDuplicateSkill.
func Prefixed(plugin string, src skill.Source) skill.Source {
	return &prefixed{prefix: plugin + ":", src: src}
}

// BareName strips a "plugin:" qualifier, or returns name unchanged if it
// carries none - the config-compatibility form Scoped matches against.
func BareName(name string) string {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[i+1:]
	}
	return name
}

type prefixed struct {
	prefix string
	src    skill.Source
}

func (p *prefixed) strip(name string) (string, bool) {
	return strings.CutPrefix(name, p.prefix)
}

func (p *prefixed) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	fms, err := p.src.ListFrontmatters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*skill.Frontmatter, len(fms))
	for i, fm := range fms {
		cp := *fm
		cp.Name = p.prefix + fm.Name
		out[i] = &cp
	}
	return out, nil
}

func (p *prefixed) ListResources(ctx context.Context, name, subpath string) ([]string, error) {
	real, ok := p.strip(name)
	if !ok {
		return nil, skill.ErrSkillNotFound
	}
	return p.src.ListResources(ctx, real, subpath)
}

func (p *prefixed) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	real, ok := p.strip(name)
	if !ok {
		return nil, skill.ErrSkillNotFound
	}
	fm, err := p.src.LoadFrontmatter(ctx, real)
	if err != nil {
		return nil, err
	}
	cp := *fm
	cp.Name = name
	return &cp, nil
}

func (p *prefixed) LoadInstructions(ctx context.Context, name string) (string, error) {
	real, ok := p.strip(name)
	if !ok {
		return "", skill.ErrSkillNotFound
	}
	return p.src.LoadInstructions(ctx, real)
}

func (p *prefixed) LoadResource(ctx context.Context, name, resourcePath string) (io.ReadCloser, error) {
	real, ok := p.strip(name)
	if !ok {
		return nil, skill.ErrSkillNotFound
	}
	return p.src.LoadResource(ctx, real, resourcePath)
}

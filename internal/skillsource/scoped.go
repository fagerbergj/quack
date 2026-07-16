package skillsource

import (
	"context"
	"io"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// Scoped restricts src to only the named skills — an agent's declared skill
// scope (config.AgentConfig.Skills / config.OrchestratorConfig.Skills). A name
// outside the scope behaves exactly as if it didn't exist (ErrSkillNotFound),
// so an out-of-scope load_skill fails the same way a typo'd name would.
//
// Apply Scoped to the BUILT-IN source only, before wrapping it with New — a
// cloned repo's project skills (arbitrary, unknown at config time) stay fully
// additive and unrestricted, and built-in still wins any collision.
func Scoped(src skill.Source, names []string) skill.Source {
	allow := make(map[string]bool, len(names))
	for _, n := range names {
		allow[n] = true
	}
	return &scoped{src: src, allow: allow}
}

type scoped struct {
	src   skill.Source
	allow map[string]bool
}

func (s *scoped) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	all, err := s.src.ListFrontmatters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*skill.Frontmatter, 0, len(all))
	for _, fm := range all {
		if s.allow[fm.Name] {
			out = append(out, fm)
		}
	}
	return out, nil
}

func (s *scoped) ListResources(ctx context.Context, name, subpath string) ([]string, error) {
	if !s.allow[name] {
		return nil, skill.ErrSkillNotFound
	}
	return s.src.ListResources(ctx, name, subpath)
}

func (s *scoped) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	if !s.allow[name] {
		return nil, skill.ErrSkillNotFound
	}
	return s.src.LoadFrontmatter(ctx, name)
}

func (s *scoped) LoadInstructions(ctx context.Context, name string) (string, error) {
	if !s.allow[name] {
		return "", skill.ErrSkillNotFound
	}
	return s.src.LoadInstructions(ctx, name)
}

func (s *scoped) LoadResource(ctx context.Context, name, resourcePath string) (io.ReadCloser, error) {
	if !s.allow[name] {
		return nil, skill.ErrSkillNotFound
	}
	return s.src.LoadResource(ctx, name, resourcePath)
}

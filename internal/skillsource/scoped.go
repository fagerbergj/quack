package skillsource

import (
	"context"
	"io"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// Scoped restricts src to an agent's declared skill scope; an out-of-scope name is ErrSkillNotFound. Apply
// it to the built-in source only: project skills stay unrestricted and built-in wins collisions.
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

// resolve: an allowed literal name wins, else the first src name whose bare form is allowed (merge order),
// so every method agrees with ListFrontmatters.
func (s *scoped) resolve(ctx context.Context, name string) (string, error) {
	// allow can itself hold a bare entry, so "in allow" alone isn't proof
	// name is real - confirm src actually has a skill by that exact name.
	if s.allow[name] {
		if _, err := s.src.LoadFrontmatter(ctx, name); err == nil {
			return name, nil
		}
	}
	bare := BareName(name)
	if !s.allow[bare] {
		return "", skill.ErrSkillNotFound
	}
	fms, err := s.src.ListFrontmatters(ctx)
	if err != nil {
		return "", err
	}
	for _, fm := range fms {
		if BareName(fm.Name) != bare {
			continue
		}
		// fm.Name is bare's first-wins resolution; a different plugin's qualified name is out of scope, never
		// redirected here.
		if name == bare || name == fm.Name {
			return fm.Name, nil
		}
		return "", skill.ErrSkillNotFound
	}
	return "", skill.ErrSkillNotFound
}

func (s *scoped) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	all, err := s.src.ListFrontmatters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*skill.Frontmatter, 0, len(all))
	seenBare := map[string]bool{}
	for _, fm := range all {
		if s.allow[fm.Name] {
			out = append(out, fm)
			continue
		}
		bare := BareName(fm.Name)
		// A bare config name matches the FIRST plugin providing it (source
		// order = merge order), not every plugin that happens to ship it.
		if s.allow[bare] && !seenBare[bare] {
			seenBare[bare] = true
			out = append(out, fm)
		}
	}
	return out, nil
}

func (s *scoped) ListResources(ctx context.Context, name, subpath string) ([]string, error) {
	resolved, err := s.resolve(ctx, name)
	if err != nil {
		return nil, skill.ErrSkillNotFound
	}
	return s.src.ListResources(ctx, resolved, subpath)
}

func (s *scoped) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	resolved, err := s.resolve(ctx, name)
	if err != nil {
		return nil, skill.ErrSkillNotFound
	}
	return s.src.LoadFrontmatter(ctx, resolved)
}

func (s *scoped) LoadInstructions(ctx context.Context, name string) (string, error) {
	resolved, err := s.resolve(ctx, name)
	if err != nil {
		return "", skill.ErrSkillNotFound
	}
	return s.src.LoadInstructions(ctx, resolved)
}

func (s *scoped) LoadResource(ctx context.Context, name, resourcePath string) (io.ReadCloser, error) {
	resolved, err := s.resolve(ctx, name)
	if err != nil {
		return nil, skill.ErrSkillNotFound
	}
	return s.src.LoadResource(ctx, resolved, resourcePath)
}

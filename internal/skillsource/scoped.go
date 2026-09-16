package skillsource

import (
	"context"
	"io"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// Scoped restricts src to only the named skills - an agent's declared
// skill scope (config.AgentConfig.Skills / config.OrchestratorConfig.Skills). A name outside the scope behaves exactly as if it didn't exist (ErrSkillNotFound), so an out-of-scope load_skill fails the same way a typo'd name would.
//
// Apply Scoped to the BUILT-IN source only, before wrapping it with New - a cloned repo's project skills (arbitrary, unknown at config time) stay fully additive and unrestricted, and built-in still wins any collision.
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

// resolve maps name to the name to forward to src: a real allowed literal
// name wins, else the FIRST src name whose bare form is allowed (merge
// order) - keeps every method agreeing with ListFrontmatters (#1427 F3).
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
		// fm.Name is the first-wins resolution of bare. A caller that asked
		// for a DIFFERENT plugin's qualified name is out of scope, never
		// silently redirected to this one (#1427 R2).
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

// Package skillsource adds project-level skills from cloned repos (.agents/skills/ + .claude/skills/)
// on top of the built-in library. Built-in always wins name collisions; paths resolve through the jail.
package skillsource

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/workspace"
)

// projectSkillDirs: conventional project-skill locations, .agents before .claude.
var projectSkillDirs = []string{".agents/skills", ".claude/skills"}

// New wraps builtin with a project-aware layer; jail nil ⇒ builtin unchanged.
func New(builtin skill.Source, jail *workspace.Jail, userID string) skill.Source {
	if jail == nil {
		return builtin
	}
	return &projectAware{builtin: builtin, jail: jail, userID: userID}
}

// sourcesUnder returns a FileSystemSource for each existing project skills dir under the scope.
func sourcesUnder(jail *workspace.Jail, userID, chatID, repoRel string) []skill.Source {
	if jail == nil {
		return nil
	}
	clean := filepath.Clean(repoRel)
	if clean == "." || clean == "" {
		return nil
	}
	var out []skill.Source
	for _, sub := range projectSkillDirs {
		real, err := jail.Resolve(userID, chatID, filepath.Join(clean, sub))
		if err != nil {
			continue // escapes the jail, or bad user/chat id - skip
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			continue // no such skills dir in this repo
		}
		out = append(out, Tolerant(NewFileSystemSource(os.DirFS(real)), os.DirFS(real), real))
	}
	return out
}

// Tolerant wraps src (backed by fsys) so ListFrontmatters skips a single
// malformed skill instead of failing the whole source (#1080: one plugin's
// new frontmatter field crash-looped the server at startup). label names
// fsys in the warning log - a real path, or a descriptive name when fsys has
// none (an embedded FS).
func Tolerant(src skill.Source, fsys fs.FS, label string) skill.Source {
	return &tolerant{Source: src, fsys: fsys, label: label}
}

// tolerant: a skill.Source that lists per-entry so one malformed skill doesn't abort the whole source.
type tolerant struct {
	skill.Source
	fsys  fs.FS
	label string // path or descriptive name of fsys, for the warning
}

// warnedBadSkills dedupes malformed-skill warnings to one line per path.
var warnedBadSkills sync.Map // skill dir path → struct{}

func (t *tolerant) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	entries, err := fs.ReadDir(t.fsys, ".")
	if err != nil {
		return nil, nil // unreadable skills dir: no skills, not an error
	}
	var out []*skill.Frontmatter
	for _, e := range entries {
		if !e.IsDir() {
			continue // skills are directories (dir/SKILL.md)
		}
		fm, err := t.Source.LoadFrontmatter(ctx, e.Name())
		if err != nil {
			if !errors.Is(err, skill.ErrSkillNotFound) { // a dir without SKILL.md simply isn't a skill
				path := filepath.Join(t.label, e.Name(), "SKILL.md")
				if _, dup := warnedBadSkills.LoadOrStore(path, struct{}{}); !dup {
					slog.Warn("skipping malformed skill; other skills still load",
						"component", "skillsource", "path", path, "err", err)
				}
			}
			continue
		}
		out = append(out, fm)
	}
	return out, nil
}

// project builds a fresh source over ALL project skills in the jail (walks both levels; rebuilt per query).
func (p *projectAware) project() skill.Source { return skill.NewMergedSource(p.sources()...) }

// sources returns one tolerant source per project skills dir.
func (p *projectAware) sources() []skill.Source {
	root, err := p.jail.Resolve(p.userID, "", "")
	if err != nil {
		return nil
	}
	scopes, err := os.ReadDir(root)
	if err != nil {
		return nil // user root not created yet (no clones): no project skills
	}
	var sources []skill.Source
	for _, scope := range scopes {
		if !scope.IsDir() || scope.Name()[0] == '.' {
			continue // skip files and dot-dirs (e.g. .quack-home)
		}
		chatID := scope.Name()
		scopeRoot, err := p.jail.Resolve(p.userID, chatID, "")
		if err != nil {
			continue
		}
		// A chat scope's children are per-node working dirs; repos live inside one.
		for _, child := range readDirs(scopeRoot) {
			sources = append(sources, sourcesUnder(p.jail, p.userID, chatID, child)...)
			nodeRoot, err := p.jail.Resolve(p.userID, chatID, child)
			if err != nil {
				continue
			}
			for _, repo := range readDirs(nodeRoot) {
				sources = append(sources, sourcesUnder(p.jail, p.userID, chatID, child+"/"+repo)...)
			}
		}
	}
	return sources
}

// readDirs lists sub-directory names, skipping dot-dirs and files.
func readDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// projectAware serves built-in skills first, additive project skills behind; built-in wins collisions.
type projectAware struct {
	builtin skill.Source
	jail    *workspace.Jail
	userID  string
}

// ListFrontmatters: built-in plus non-colliding project skills; malformed skills are skipped, not fatal.
func (p *projectAware) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	builtin, err := p.builtin.ListFrontmatters(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(builtin))
	for _, fm := range builtin {
		seen[fm.Name] = true
	}
	out := builtin
	for _, src := range p.sources() {
		proj, perr := src.ListFrontmatters(ctx)
		if perr != nil {
			slog.Warn("project skills dir unavailable; skipping it",
				"component", "skillsource", "err", perr)
			continue
		}
		for _, fm := range proj {
			if seen[fm.Name] {
				continue // first listing wins the collision; hide the duplicate
			}
			seen[fm.Name] = true
			out = append(out, fm)
		}
	}
	return out, nil
}

// All Load* try built-in first, falling back to project on ErrSkillNotFound.

func (p *projectAware) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	fm, err := p.builtin.LoadFrontmatter(ctx, name)
	if err == nil || !errors.Is(err, skill.ErrSkillNotFound) {
		return fm, err
	}
	return p.project().LoadFrontmatter(ctx, name)
}

func (p *projectAware) LoadInstructions(ctx context.Context, name string) (string, error) {
	ins, err := p.builtin.LoadInstructions(ctx, name)
	if err == nil || !errors.Is(err, skill.ErrSkillNotFound) {
		return ins, err
	}
	return p.project().LoadInstructions(ctx, name)
}

func (p *projectAware) LoadResource(ctx context.Context, name, resourcePath string) (io.ReadCloser, error) {
	rc, err := p.builtin.LoadResource(ctx, name, resourcePath)
	if err == nil || !errors.Is(err, skill.ErrSkillNotFound) {
		return rc, err
	}
	return p.project().LoadResource(ctx, name, resourcePath)
}

func (p *projectAware) ListResources(ctx context.Context, name, subpath string) ([]string, error) {
	res, err := p.builtin.ListResources(ctx, name, subpath)
	if err == nil || !errors.Is(err, skill.ErrSkillNotFound) {
		return res, err
	}
	return p.project().ListResources(ctx, name, subpath)
}

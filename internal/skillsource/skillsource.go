// Package skillsource makes an agent's skill library project-aware: on top of
// the fixed built-in library (quack's shipped skills/ + the vendored ponytail
// submodule), it ALSO surfaces the project-level skills a cloned repository
// defines under its own .agents/skills/ and .claude/skills/ directories, so an
// agent working in a repo can load_skill that repo's skills.
//
// The working repo is DYNAMIC — a per-user jailed clone the server never knew
// about at startup — so this is a skill.Source that scans the jail's workspace
// at QUERY time (not a fixed startup source). Two rules keep it safe and lazy:
//
//   - Precedence: the built-in library ALWAYS wins a name collision. A cloned
//     (untrusted) repo must not be able to hijack a core skill like `plan-work`
//     or `ponytail` by shadowing its name. Project skills are purely ADDITIVE —
//     a colliding project skill is silently HIDDEN, never an error (unlike a
//     static skill.NewMergedSource, where a duplicate name is a startup
//     failure — the right loudness for a vendoring mistake, the wrong loudness
//     for arbitrary repo contents).
//   - Jail discipline: every path is resolved THROUGH the jail (symlink-
//     resolved, containment-checked), so discovery only ever reads under the
//     jailed workspace.
//
// Ceiling (ponytail): the source discovers project skills only in the
// IMMEDIATE child repos of the jail root (<root>/<repo>/.agents|.claude/skills)
// — the clone-and-work-in-it case. A repo cloned into a nested subdir, or a
// monorepo's sub-project skills, are out of scope; add a bounded walk here if
// that ever matters. And os.DirFS follows symlinks WITHIN a resolved skills
// dir, so a fully hostile repo could symlink a skill file out of the jail; the
// dir root is jail-contained, deeper per-file symlink hardening is deferred
// (single-user, self-cloned workspace — low severity).
package skillsource

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/workspace"
)

// projectSkillDirs are the two conventional project-skill locations, relative
// to a repo root. Order is precedence WITHIN a repo (.agents before .claude),
// mirroring the built-in library's own layout.
var projectSkillDirs = []string{".agents/skills", ".claude/skills"}

// New wraps builtin with a project-aware layer: built-in skills first (they win
// any collision), plus additive project skills discovered under the jail's
// cloned repos at query time. jail nil ⇒ builtin is returned unchanged (no
// workspace, no project skills).
func New(builtin skill.Source, jail *workspace.Jail, userID string) skill.Source {
	if jail == nil {
		return builtin
	}
	return &projectAware{builtin: builtin, jail: jail, userID: userID}
}

// ProjectSkills lists the project skills discovered for ONE repo root (a
// jail-relative path, e.g. "myrepo"), for the `cd` tool's on-entry report.
// Best-effort: a missing dir, a malformed skill, or a jail-escape is skipped,
// never surfaced as an error (a bad skill in a cloned repo must not break `cd`).
func ProjectSkills(jail *workspace.Jail, userID, repoRel string) []*skill.Frontmatter {
	src := skill.NewMergedSource(sourcesUnder(jail, userID, repoRel)...)
	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		return nil
	}
	return fms
}

// sourcesUnder returns a FileSystemSource for whichever of repoRel's
// .agents/skills / .claude/skills directories exist, each resolved THROUGH the
// jail so the dir is symlink-contained. A dir that is absent or escapes the
// jail is skipped. repoRel "" or "." (the jail root itself) has no containing
// repo, so it yields nothing.
func sourcesUnder(jail *workspace.Jail, userID, repoRel string) []skill.Source {
	if jail == nil {
		return nil
	}
	clean := filepath.Clean(repoRel)
	if clean == "." || clean == "" {
		return nil
	}
	var out []skill.Source
	for _, sub := range projectSkillDirs {
		real, err := jail.Resolve(userID, filepath.Join(clean, sub))
		if err != nil {
			continue // escapes the jail, or bad user id — skip
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			continue // no such skills dir in this repo
		}
		out = append(out, skill.NewFileSystemSource(os.DirFS(real)))
	}
	return out
}

// project builds a fresh source over ALL project skills currently in the jail:
// one per immediate child repo of the jail root. Rebuilt per query so a repo
// cloned mid-run is picked up without restart. Never errors: an unreadable jail
// root yields an empty (skill-less) source.
func (p *projectAware) project() skill.Source {
	root, err := p.jail.Resolve(p.userID, "")
	if err != nil {
		return skill.NewMergedSource()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return skill.NewMergedSource() // jail root not created yet (no clones): no project skills
	}
	var sources []skill.Source
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue // skip files and dot-dirs (e.g. .quack-home)
		}
		sources = append(sources, sourcesUnder(p.jail, p.userID, e.Name())...)
	}
	return skill.NewMergedSource(sources...)
}

// projectAware is a skill.Source that serves built-in skills first and additive
// project skills (discovered in the jail) behind them, with the built-in
// winning every name collision.
type projectAware struct {
	builtin skill.Source
	jail    *workspace.Jail
	userID  string
}

// ListFrontmatters returns the built-in skills plus every project skill whose
// name does NOT collide with a built-in one (built-in wins; the project
// duplicate is hidden). A failure to list project skills (a malformed skill in
// a cloned repo) is logged and treated as "no project skills" — it must not
// break listing the built-in library.
func (p *projectAware) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	builtin, err := p.builtin.ListFrontmatters(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(builtin))
	for _, fm := range builtin {
		seen[fm.Name] = true
	}
	proj, perr := p.project().ListFrontmatters(ctx)
	if perr != nil {
		slog.Warn("project skills unavailable; listing built-in skills only",
			"component", "skillsource", "err", perr)
		return builtin, nil
	}
	out := builtin
	for _, fm := range proj {
		if seen[fm.Name] {
			continue // built-in wins the collision; hide the project duplicate
		}
		seen[fm.Name] = true
		out = append(out, fm)
	}
	return out, nil
}

// LoadFrontmatter, LoadInstructions, LoadResource, and ListResources all try
// the built-in library FIRST (so it wins any name collision) and fall back to
// the project source only on ErrSkillNotFound.

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

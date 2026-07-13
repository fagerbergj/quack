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
	"sync"

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
// chat-root-relative path, e.g. "myrepo") within a given per-chat scope, for the
// `cd` tool's on-entry report. chatID scopes discovery to the calling chat's
// workspace (<root>/<user>/<chatID>/<repoRel>/…) so `cd`'s report matches where
// the worker actually cloned. Best-effort: a missing dir, a malformed skill, or
// a jail-escape is skipped, never surfaced as an error (a bad skill in a cloned
// repo must not break `cd`).
func ProjectSkills(jail *workspace.Jail, userID, chatID, repoRel string) []*skill.Frontmatter {
	src := skill.NewMergedSource(sourcesUnder(jail, userID, chatID, repoRel)...)
	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		return nil
	}
	return fms
}

// sourcesUnder returns a FileSystemSource for whichever of repoRel's
// .agents/skills / .claude/skills directories exist under the (userID, chatID)
// scope, each resolved THROUGH the jail so the dir is symlink-contained. A dir
// that is absent or escapes the jail is skipped. repoRel "" or "." (the scope
// root itself) has no containing repo, so it yields nothing.
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
			continue // escapes the jail, or bad user/chat id — skip
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			continue // no such skills dir in this repo
		}
		out = append(out, &tolerant{Source: skill.NewFileSystemSource(os.DirFS(real)), dir: real})
	}
	return out
}

// tolerant is a skill.Source whose listing survives a malformed skill. ADK's
// FileSystemSource.ListFrontmatters aborts the WHOLE directory on the first
// SKILL.md that fails frontmatter validation — so in a world of cloned
// third-party repos, one bad file silently costs the agent every project skill
// (and re-fails on every skill call). This lists per-directory instead: a skill
// that parses is returned, one that doesn't is skipped and reported ONCE per
// path. Everything else (Load*) delegates unchanged: naming a broken skill
// explicitly still surfaces its real error.
type tolerant struct {
	skill.Source
	dir string // real (jail-resolved) path of the skills dir, for the warning
}

// warnedBadSkills dedupes the malformed-skill warning to one line per path, for
// the process lifetime — the listing is rebuilt on every skill call, so without
// this the same bad file warns hundreds of times in a single run.
var warnedBadSkills sync.Map // skill dir path → struct{}

func (t *tolerant) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	entries, err := os.ReadDir(t.dir)
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
				path := filepath.Join(t.dir, e.Name(), "SKILL.md")
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

// project builds a fresh source over ALL project skills currently in the jail.
// Repos now live one level deeper — under each per-chat scope
// (<root>/<user>/<chat>/<repo>) — but this source is the shared startup
// skilltoolset singleton (list_skills/load_skill), built once and driven by a
// plain context.Context, so it CANNOT recover the calling chat id the way the
// workspace tools do (there is no advisor-thread marker on a context.Context).
// It therefore walks BOTH levels: each per-chat scope dir, then each repo dir
// within it. Skill names thus become visible across a single user's chats in
// list_skills — read-only, single-user, acceptable; the `cd` tool's report
// (ProjectSkills, threaded with the real chat id) is the per-chat-accurate
// surface. Rebuilt per query so a repo cloned mid-run is picked up without
// restart. Never errors: an unreadable root yields an empty source.
func (p *projectAware) project() skill.Source { return skill.NewMergedSource(p.sources()...) }

// sources returns one tolerant source per project skills dir currently in the
// jail (see project()).
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
		repos, err := os.ReadDir(scopeRoot)
		if err != nil {
			continue
		}
		for _, repo := range repos {
			if !repo.IsDir() || repo.Name()[0] == '.' {
				continue
			}
			sources = append(sources, sourcesUnder(p.jail, p.userID, chatID, repo.Name())...)
		}
	}
	return sources
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
// name does NOT collide with an already-listed one (built-in wins; a project
// duplicate — including the same skill name in two clones — is hidden, never an
// error). Listing is per source and per file: a malformed SKILL.md anywhere in
// the jail is skipped (warned once, by tolerant), and every skill that parsed is
// still returned. One bad file in one cloned repo must never cost the agent its
// skills.
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

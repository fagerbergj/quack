package acp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/workspace"
)

// maxEnvironmentEntries bounds the top-level entry listing in the environment
// block - a pathological directory (an agent that wrote thousands of files at
// its root) must never blow the round's context window just to say "here's your cwd".
const maxEnvironmentEntries = 200

// environmentBlock renders a FACTUAL, Codex-CLI-style <environment_context>
// grounding the round's prompt: absolute cwd, whether it's a git repo (branch
// + short HEAD sha when so), and the top-level entries. Observation, not instruction - this is what replaces the old "do not clone the repo, it's already here" prose (agents/code-explorer/prompt.md): prose asserting where the repo is competes with a task naming one and loses; a plain fact about the actual filesystem does not compete with anything. Deterministic given (cwd, repo state), so it costs nothing to include on every round.
func environmentBlock(ctx context.Context, res *artifactsrc.Resolver, cwd string, caps workspace.Caps) string {
	f := envFacts{Cwd: cwd, MaxEntries: maxEnvironmentEntries, ReadOnly: caps.ReadOnly}
	f.Branch, f.Sha, f.Git = gitInfo(ctx, cwd, caps)
	entries, truncated := topLevelEntries(cwd)
	f.Entries, f.Truncated = strings.Join(entries, ", "), truncated
	if caps.ReadOnly {
		// landlock and bwrap both enforce this. Name which paths, not what to do
		// with them: an agent told only "read-only" either burns a round on
		// EACCES or gives up on running the change. Naming the writable paths is what makes "run it" achievable here.
		writable := []string{workspace.SandboxTmpDir(caps)}
		if caps.HomeDir != "" {
			writable = append(writable, caps.HomeDir)
		}
		f.Writable = strings.Join(writable, ", ")
	}
	out, err := renderEnvironment(ctx, res, f)
	if err != nil {
		// Cosmetic grounding: degrade to no block rather than fail the round,
		// same as gitInfo degrading to "git: no".
		slog.Warn("acp: environment block unavailable", "component", "acp", "err", err)
		return ""
	}
	return out
}

// envFacts is system/acp.environment's template data.
type envFacts struct {
	Cwd        string
	Git        bool
	Branch     string
	Sha        string
	Entries    string
	Truncated  bool
	MaxEntries int
	ReadOnly   bool
	Writable   string
}

// envTemplates caches the parsed system/acp.environment per version.
var envTemplates artifactsrc.TemplateCache

func renderEnvironment(ctx context.Context, res *artifactsrc.Resolver, f envFacts) (string, error) {
	var b strings.Builder
	// A stored version that will not render falls back to the shipped file
	// (artifactsrc.Render) - a typo must not silently drop the whole block.
	if _, err := artifactsrc.Render(ctx, res, &envTemplates, "system/acp.environment", func(t *template.Template) error {
		b.Reset()
		return t.Execute(&b, f)
	}); err != nil {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// gitInfo reports cwd's current branch and short HEAD sha via the SAME
// sandboxed git path every other repo read uses (workspace.RunArgv) - so a
// linked worktree gets the same landlock/bwrap grants as any other git command run there. ok=false for a non-repo cwd (the common case for a non-code node) or any git failure - the block degrades to "git: no" rather than failing the round over a cosmetic line.
func gitInfo(ctx context.Context, cwd string, caps workspace.Caps) (branch, sha string, ok bool) {
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err != nil {
		return "", "", false
	}
	res, err := workspace.RunArgv(ctx, cwd, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}, caps)
	if err != nil || res.ExitCode != 0 {
		return "", "", false
	}
	branch = strings.TrimSpace(res.Output)
	if res2, err := workspace.RunArgv(ctx, cwd, []string{"git", "rev-parse", "--short", "HEAD"}, caps); err == nil && res2.ExitCode == 0 {
		sha = strings.TrimSpace(res2.Output)
	}
	return branch, sha, true
}

// topLevelEntries lists cwd's immediate entries (name only, directories
// suffixed "/"), sorted, bounded to maxEnvironmentEntries. "" (empty, false)
// for an unreadable cwd - a node whose worker hasn't written anything yet.
func topLevelEntries(cwd string) (entries []string, truncated bool) {
	des, err := os.ReadDir(cwd)
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(des))
	for _, d := range des {
		name := d.Name()
		if d.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxEnvironmentEntries {
		return names[:maxEnvironmentEntries], true
	}
	return names, false
}

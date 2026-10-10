package acp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/workspace"
)

// maxEnvironmentEntries bounds the top-level listing so a directory with thousands of root files
// can't blow the round's context window.
const maxEnvironmentEntries = 200

// environmentBlock renders <environment_context>: cwd, git state, top-level entries and the sandbox facts agents
// would otherwise rediscover each round. Deterministic given (cwd, repo state, caps), so cheap every round.
func environmentBlock(ctx context.Context, res *artifactsrc.Resolver, cwd string, caps workspace.Caps) (string, artifactsrc.Artifact) {
	f := envFacts{
		Cwd: cwd, MaxEntries: maxEnvironmentEntries, ReadOnly: caps.ReadOnly,
		GoModCache:          caps.Env["GOMODCACHE"],
		GoModCachePreseeded: workspace.GoModCachePreseeded(caps.Env["GOMODCACHE"]),
		Sandboxed:           caps.Sandbox == workspace.SandboxBwrap || caps.Sandbox == workspace.SandboxLandlock,
	}
	f.Branch, f.Sha, f.Git = gitInfo(ctx, cwd, caps)
	entries, truncated := topLevelEntries(cwd)
	f.Entries, f.Truncated = strings.Join(entries, ", "), truncated
	if caps.ReadOnly {
		// Name the writable paths, not just "read-only": without them an agent burns a round on EACCES
		// or gives up on running the change. landlock and bwrap both enforce this.
		writable := []string{workspace.SandboxTmpDir(caps)}
		if caps.HomeDir != "" {
			writable = append(writable, caps.HomeDir)
		}
		f.Writable = strings.Join(writable, ", ")
	}
	out, art, err := renderEnvironment(ctx, res, f)
	if err != nil {
		// Cosmetic grounding: degrade to no block rather than fail the round,
		// same as gitInfo degrading to "git: no".
		slog.Warn("acp: environment block unavailable", "component", "acp", "err", err)
		return "", artifactsrc.Artifact{}
	}
	return out, art
}

// envFacts is system/acp.environment's template data.
type envFacts struct {
	Cwd                 string
	Git                 bool
	Branch              string
	Sha                 string
	Entries             string
	Truncated           bool
	MaxEntries          int
	ReadOnly            bool
	Writable            string
	GoModCache          string
	GoModCachePreseeded bool
	Sandboxed           bool
}

// envTemplates caches the parsed system/acp.environment per version.
var envTemplates artifactsrc.TemplateCache

func renderEnvironment(ctx context.Context, res *artifactsrc.Resolver, f envFacts) (string, artifactsrc.Artifact, error) {
	var b strings.Builder
	// A stored version that will not render falls back to the shipped file
	// (artifactsrc.Render) - a typo must not silently drop the whole block.
	art, err := artifactsrc.Render(ctx, res, &envTemplates, "system/acp.environment", func(t *template.Template) error {
		b.Reset()
		return t.Execute(&b, f)
	})
	if err != nil {
		return "", artifactsrc.Artifact{}, err
	}
	return strings.TrimRight(b.String(), "\n"), art, nil
}

// gitInfo reports cwd's branch and short sha, run inside the round's own sandbox: the repo is agent-written.
// ok=false for a non-repo cwd or any failure, sandbox included, so the block degrades to "git: no".
func gitInfo(ctx context.Context, cwd string, caps workspace.Caps) (branch, sha string, ok bool) {
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err != nil {
		return "", "", false
	}
	revParse := func(arg string) []string {
		return append(workspace.GitSafeArgs(), "rev-parse", arg, "HEAD")
	}
	res, err := workspace.RunArgv(ctx, cwd, append([]string{"git"}, revParse("--abbrev-ref")...), caps)
	if err != nil || res.ExitCode != 0 {
		return "", "", false
	}
	branch = strings.TrimSpace(res.Output)
	if res2, err := workspace.RunArgv(ctx, cwd, append([]string{"git"}, revParse("--short")...), caps); err == nil && res2.ExitCode == 0 {
		sha = strings.TrimSpace(res2.Output)
	}
	return branch, sha, true
}

// topLevelEntries lists cwd's entries (dirs suffixed "/"), sorted and bounded to maxEnvironmentEntries;
// empty for an unreadable cwd (a worker that hasn't written anything yet).
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
	slices.Sort(names)
	if len(names) > maxEnvironmentEntries {
		return names[:maxEnvironmentEntries], true
	}
	return names, false
}

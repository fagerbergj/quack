package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// maxEnvironmentEntries bounds the top-level entry listing in the environment
// block - a pathological directory (an agent that wrote thousands of files at
// its root) must never blow the round's context window just to say "here's
// your cwd".
const maxEnvironmentEntries = 200

// environmentBlock renders a FACTUAL, Codex-CLI-style <environment_context>
// grounding the round's prompt: absolute cwd, whether it's a git repo (branch
// + short HEAD sha when so), and the top-level entries. Observation, not
// instruction - this is what replaces the old "do not clone the repo, it's
// already here" prose (agents/code-explorer/prompt.md): prose asserting
// where the repo is competes with a task naming one and loses; a plain fact
// about the actual filesystem does not compete with anything. Deterministic
// given (cwd, repo state), so it costs nothing to include on every round.
func environmentBlock(ctx context.Context, cwd string, caps workspace.Caps) string {
	var b strings.Builder
	b.WriteString("<environment_context>\n")
	fmt.Fprintf(&b, "cwd: %s\n", cwd)
	if branch, sha, ok := gitInfo(ctx, cwd, caps); ok {
		if sha == "" {
			fmt.Fprintf(&b, "git: yes (branch %s)\n", branch)
		} else {
			fmt.Fprintf(&b, "git: yes (branch %s, HEAD %s)\n", branch, sha)
		}
	} else {
		b.WriteString("git: no\n")
	}
	entries, truncated := topLevelEntries(cwd)
	switch {
	case len(entries) == 0:
		b.WriteString("entries: (none - empty or unreadable)\n")
	case truncated:
		fmt.Fprintf(&b, "entries (first %d): %s\n", maxEnvironmentEntries, strings.Join(entries, ", "))
	default:
		fmt.Fprintf(&b, "entries: %s\n", strings.Join(entries, ", "))
	}
	if caps.ReadOnly {
		// landlock and bwrap both enforce this. Name which paths, not what to do
		// with them: an agent told only "read-only" either burns a round on
		// EACCES or gives up on running the change. Naming the writable paths is
		// what makes "run it" achievable here.
		fmt.Fprintf(&b, "filesystem: read-only (OS-enforced, EACCES on write): %s\n", cwd)
		writable := []string{workspace.SandboxTmpDir(caps)}
		if caps.HomeDir != "" {
			writable = append(writable, caps.HomeDir)
		}
		fmt.Fprintf(&b, "filesystem: writable: %s\n", strings.Join(writable, ", "))
		b.WriteString("reads and execution work anywhere; in-tree writes (npm install, go build artifacts, file edits) fail. To run code against this tree, copy or `git clone --local` it into a writable path first - a clone keeps the module path, so language-level rules like Go's internal/ visibility still resolve.\n")
	}
	b.WriteString("</environment_context>")
	return b.String()
}

// gitInfo reports cwd's current branch and short HEAD sha via the SAME
// sandboxed git path every other repo read uses (workspace.RunArgv) - so a
// linked worktree gets the same landlock/bwrap grants as any other
// git command run there. ok=false for a non-repo cwd (the common case for a
// non-code node) or any git failure - the block degrades to "git: no" rather
// than failing the round over a cosmetic line.
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

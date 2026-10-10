// The disk-truth probe: reads the clone directly since external ACP workers commit outside the session ledger.
package vetting

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"
)

// probeAugmentFromRepo: ledger event name for the probe (emitProbeEvent).
const probeAugmentFromRepo = "augment_from_repo"

// gitProbeCache memoises augmentFromRepo's git reads per (dir, HEAD sha); actFor calls it ~9 times a run.
// ponytail: process-lifetime sync.Map, never evicted; add a TTL/size cap if it shows up in profiling.
var gitProbeCache sync.Map // dir+"\x00"+head → *gitProbeResult; nil means "nothing committed yet"

// gitProbeResult is the git-derived slice of augmentFromRepo's work, replayed
// onto a fresh workerActivity on a cache hit instead of re-shelling to git.
type gitProbeResult struct {
	branch    string
	written   []string
	commitLog string
	prTitle   string
	prBody    string
}

// augmentFromRepo folds the clone's git state into session-derived activity.
func augmentFromRepo(ctx context.Context, act *workerActivity, cfg Config) {
	// Read-only reviewers/explorers don't commit; staging a PR would reset the reviewed branch.
	if cfg.ReadOnly {
		return
	}
	if cfg.Setup == nil || cfg.Workspace == nil || act.committed {
		return
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil || !isDir(filepath.Join(dir, ".git")) {
		return
	}

	caps := checksCaps(cfg)
	head := gitLine(dir, caps, "rev-parse", "HEAD")
	if head != "" {
		if v, ok := gitProbeCache.Load(dir + "\x00" + head); ok {
			applyGitProbe(act, v.(*gitProbeResult), cfg)
			return
		}
	}

	var result map[string]any
	var probeErr error
	defer func() { otelobs.EmitToolCall(ctx, probeScope, probeAugmentFromRepo, nil, result, probeErr) }()

	base, err := baseCommit(dir, caps)
	if err != nil {
		probeErr = err
		return
	}
	if head == "" || head == base {
		result = map[string]any{"committed": false}
		if head != "" {
			gitProbeCache.Store(dir+"\x00"+head, (*gitProbeResult)(nil))
		}
		return
	}

	changed := gitLines(dir, caps, "diff", "--name-only", base, head)
	gp := buildGitProbe(dir, caps, cfg, base, head, changed)
	gitProbeCache.Store(dir+"\x00"+head, gp)
	applyGitProbe(act, gp, cfg)
	result = map[string]any{"committed": true, "branch": act.currentBranch, "files_changed": len(changed)}
}

// buildGitProbe assembles the git-derived probe result for base..head: changed
// files (node-dir scoped), branch, commit log, and the PR handoff.
func buildGitProbe(dir string, caps workspace.Caps, cfg Config, base, head string, changed []string) *gitProbeResult {
	nodeDir := workspace.NodeDir(cfg.NodeID)
	gp := &gitProbeResult{}
	for _, f := range changed {
		gp.written = append(gp.written, joinWritten(nodeDir, f))
	}
	if br := gitLine(dir, caps, "rev-parse", "--abbrev-ref", "HEAD"); br != "" && br != "HEAD" {
		gp.branch = br
	}
	gp.commitLog = fmt.Sprintf(
		"git_commit(disk probe) → head=%q, files_changed=%d (commits found in the clone itself; the worker commits with its own git)",
		shortSHA(head, 12), len(changed))

	// Delivery handoff for a terminal node with no stage_pr/stage_push call: stage the PR from commits.
	if cfg.Deliver != nil {
		gp.prTitle = gitLine(dir, caps, "log", "-1", "--format=%s")
		body := strings.Join(gitLines(dir, caps, "log", "--reverse", "--format=- %s", base+".."+head), "\n")
		// Fallback body overridden by stage_pr/stage_push via augmentFromPRStage.
		gp.prBody = "Commits:\n" + body
	}
	return gp
}

// applyGitProbe replays a cached or fresh git probe onto act; gp == nil replays "nothing committed yet".
func applyGitProbe(act *workerActivity, gp *gitProbeResult, cfg Config) {
	if gp == nil {
		return
	}
	act.committed = true
	if gp.branch != "" {
		act.currentBranch = gp.branch
	}
	for _, rel := range gp.written {
		if !act.paths[rel] {
			act.paths[rel] = true
			act.written = append(act.written, rel)
		}
	}
	act.workspace = append(act.workspace, wsOp{tool: "git_commit", detail: gp.commitLog})

	if cfg.Deliver != nil {
		if act.stagedDelivery == nil {
			act.stagedDelivery = map[string]StagedDelivery{}
		}
		if _, staged := act.stagedDelivery["pr"]; !staged {
			// Key "pr" (staging slot); Kind "pull_request" (delivery discriminator).
			act.stagedDelivery["pr"] = StagedDelivery{
				Kind:   "pull_request",
				Branch: act.currentBranch,
				Title:  gp.prTitle,
				Body:   gp.prBody,
			}
		}
	}
}

// diffTruncatedMarker caps a diff section at changedFilesBudget.
const diffTruncatedMarker = "\n… (diff truncated)\n"

// diffSince returns the base...HEAD diff off the clone (base = reflog oldest), truncated. "" on failure.
func diffSince(cfg Config) (diff, base, head string) {
	if cfg.Setup == nil || cfg.Workspace == nil {
		return "", "", ""
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil || !isDir(filepath.Join(dir, ".git")) {
		return "", "", ""
	}
	caps := checksCaps(cfg)
	// This node's own starting point when we have it; the reflog base otherwise.
	b := cfg.NodeBaseSHA
	if b == "" {
		var err error
		if b, err = baseCommit(dir, caps); err != nil {
			return "", "", ""
		}
	}
	h := gitLine(dir, caps, "rev-parse", "HEAD")
	if h == "" {
		return "", "", ""
	}
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "diff", b + "..." + h}, caps)
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Output) == "" {
		return "", "", ""
	}
	out := res.Output
	if len(out) > changedFilesBudget {
		out = out[:changedFilesBudget] + diffTruncatedMarker
	}
	return out, shortSHA(b, 12), shortSHA(h, 12)
}

// buildReviewDiffSection sources the REVIEW node's changedFiles from clone diff (act.written is empty for reviewers).
func buildReviewDiffSection(cfg Config) string {
	diff, base, head := diffSince(cfg)
	if diff == "" {
		return ""
	}
	return fmt.Sprintf(
		"DIFF UNDER REVIEW (%s..%s, the actual change this review is OF - verify each finding against this diff, not the review's own description of it):\n\n%s",
		base, head, diff)
}

// buildImplementDiffSection adds the base...HEAD diff alongside full content (change-shape criteria need it).
func buildImplementDiffSection(cfg Config) string {
	diff, base, head := diffSince(cfg)
	if diff == "" {
		return ""
	}
	return fmt.Sprintf(
		"ACTUAL DIFF THIS NODE PRODUCED (%s..%s - judge what CHANGED here, not just the full file content below: is the diff minimal, does every hunk serve the task, is anything unrelated bundled in):\n\n%s",
		base, head, diff)
}

// gitLine runs one git command and returns its first output line ("", best-effort).
func gitLine(dir string, caps workspace.Caps, args ...string) string {
	lines := gitLines(dir, caps, args...)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

func gitLines(dir string, caps workspace.Caps, args ...string) []string {
	res, err := workspace.RunArgv(context.Background(), dir, append([]string{"git"}, args...), caps)
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	var out []string
	for _, l := range strings.Split(res.Output, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// commitReachable reports whether sha exists in dir's history (a force-push may drop it). It checks the
// exit status: `git cat-file` exits non-zero on a missing object.
func commitReachable(dir string, caps workspace.Caps, sha string) bool {
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "cat-file", "-e", sha + "^{commit}"}, caps)
	return err == nil && res.ExitCode == 0
}

// fileLineAt returns line (1-based) of file at sha, "" on any failure, so the finding hash degrades
// instead of blocking. It reads at sha, not disk, because HEAD may have moved past sha.
func fileLineAt(dir string, caps workspace.Caps, sha, file string, line int) string {
	if sha == "" || line <= 0 {
		return ""
	}
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "show", sha + ":" + file}, caps)
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	lines := strings.Split(res.Output, "\n")
	if line > len(lines) {
		return ""
	}
	return lines[line-1]
}

func shortSHA(sha string, n int) string { return sha[:min(n, len(sha))] }

// cloneHeadSHA is the clone's HEAD now, stamped once per node so diffSince scopes to this node's work.
// "" when there is no clone yet.
func cloneHeadSHA(cfg Config) string {
	if cfg.Setup == nil || cfg.Workspace == nil {
		return ""
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil || !isDir(filepath.Join(dir, ".git")) {
		return ""
	}
	return gitLine(dir, checksCaps(cfg), "rev-parse", "HEAD")
}

// commitHygieneOffTaskCeiling: commit_hygiene below this (normalised) means a commit swept in off-task
// files, distinct from a thin message or an incomplete on-task round.
const commitHygieneOffTaskCeiling = 0.4

// resetCloneToNodeBase drops a rejected round's commits before revise, only when commit_hygiene says they
// swept in off-task work; on-task commits stay for revise to build on. No commit_hygiene, no reset.
func resetCloneToNodeBase(cfg Config, v verdict) {
	cs, ok := v.Criteria["commit_hygiene"]
	if !ok || cs.Score >= commitHygieneOffTaskCeiling {
		return
	}
	if cfg.ReadOnly || cfg.Setup == nil || cfg.Workspace == nil || cfg.NodeBaseSHA == "" {
		return
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil || !isDir(filepath.Join(dir, ".git")) {
		return
	}
	caps := checksCaps(cfg)
	if res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "reset", "--hard", cfg.NodeBaseSHA}, caps); err != nil || res.ExitCode != 0 {
		slog.Warn("could not reset the clone before revising; the rejected round's commits may survive to delivery",
			"component", "vetting", "node", cfg.NodeID, "base", cfg.NodeBaseSHA, "err", err)
		return
	}
	if res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "clean", "-fdq"}, caps); err != nil || res.ExitCode != 0 {
		slog.Warn("could not clean untracked files left by the rejected round", "component", "vetting", "node", cfg.NodeID, "err", err)
	}
}

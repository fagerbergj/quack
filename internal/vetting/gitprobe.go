// The disk-truth probe: reads the clone directly since external ACP workers commit outside the session ledger.
package vetting

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// probeAugmentFromRepo: ledger event name for the probe (emitProbeEvent).
const probeAugmentFromRepo = "augment_from_repo"

// augmentFromRepo folds the clone's git state into session-derived activity.
// ponytail: runs a few git subprocesses per activity() call (no caching).
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

	var result map[string]any
	var probeErr error
	defer func() { emitProbeEvent(ctx, probeAugmentFromRepo, nil, result, probeErr) }()

	caps := checksCaps(cfg)
	base, err := baseCommit(dir, caps)
	if err != nil {
		probeErr = err
		return
	}
	head := gitLine(dir, caps, "rev-parse", "HEAD")
	if head == "" || head == base {
		result = map[string]any{"committed": false}
		return
	}
	act.committed = true
	if br := gitLine(dir, caps, "rev-parse", "--abbrev-ref", "HEAD"); br != "" && br != "HEAD" {
		act.currentBranch = br
	}
	nodeDir := workspace.NodeDir(cfg.NodeID)
	changed := gitLines(dir, caps, "diff", "--name-only", base, head)
	for _, f := range changed {
		rel := joinWritten(nodeDir, f)
		if !act.paths[rel] {
			act.paths[rel] = true
			act.written = append(act.written, rel)
		}
	}
	act.workspace = append(act.workspace, wsOp{tool: "git_commit", detail: fmt.Sprintf(
		"git_commit(disk probe) → head=%q, files_changed=%d (commits found in the clone itself; the worker commits with its own git)",
		short(head), len(changed))})
	result = map[string]any{"committed": true, "branch": act.currentBranch, "files_changed": len(changed)}

	// Delivery handoff for a terminal node with no stage_pr/stage_push call: stage the PR from commits.
	if cfg.Deliver != nil {
		if act.stagedDelivery == nil {
			act.stagedDelivery = map[string]StagedDelivery{}
		}
		if _, staged := act.stagedDelivery["pr"]; !staged {
			title := gitLine(dir, caps, "log", "-1", "--format=%s")
			body := strings.Join(gitLines(dir, caps, "log", "--reverse", "--format=- %s", base+".."+head), "\n")
			// Key "pr" (staging slot); Kind "pull_request" (delivery discriminator).
			act.stagedDelivery["pr"] = StagedDelivery{
				Kind:   "pull_request",
				Branch: act.currentBranch,
				Title:  title,
				// Fallback body overridden by stage_pr/stage_push via augmentFromPRStage.
				Body: "Commits:\n" + body,
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
	return out, short(b), short(h)
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

// commitReachable reports whether sha exists in dir's history (survives a
// force-push that dropped it). Exit-status aware, unlike gitLines' callers
// that only read output - `git cat-file` exits non-zero on a missing object.
func commitReachable(dir string, caps workspace.Caps, sha string) bool {
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "cat-file", "-e", sha + "^{commit}"}, caps)
	return err == nil && res.ExitCode == 0
}

// fileUnchangedSince reports whether file is byte-identical between a and b -
// the preload validity rule (#1006): `git diff --quiet` exits 0 for no diff.
func fileUnchangedSince(dir string, caps workspace.Caps, a, b, file string) bool {
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "diff", "--quiet", a, b, "--", file}, caps)
	return err == nil && res.ExitCode == 0
}

// fileLineAt returns one line (1-based) of file as it read at sha, "" on any
// failure (missing sha/file/line - the finding-hash snippet input degrades
// gracefully, never blocks the write). Used instead of reading off disk
// because HEAD may have moved past sha by the time this runs.
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

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// cloneHeadSHA is the shared clone's HEAD right now - stamped once per node so
// diffSince can scope its diff to what THIS node did (#710). "" when there is
// no clone yet, which is the single-node/no-repo case diffSince falls back on.
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

// commitHygieneOffTaskCeiling: code-implementer's commit_hygiene criterion
// scores below this (normalised) when a commit swept in files with no
// connection to the task - the contamination band, distinct from a merely
// thin commit message or an incomplete-but-on-task round (#762).
const commitHygieneOffTaskCeiling = 0.4

// resetCloneToNodeBase discards everything the just-rejected round committed,
// before the worker is re-prompted to revise - but only when commit_hygiene
// says that round swept in off-task work (#762). An ordinary incomplete or
// wrong round keeps its commits: its code is largely right, and revise is
// expected to build on it, not redo it from scratch. Keyed on the judge's
// criterion score, never on reading commits/diffs for topicality; a rubric
// that doesn't name commit_hygiene gets no reset, failing open rather than
// guessing from something else.
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

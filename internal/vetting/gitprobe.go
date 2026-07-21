// The disk-truth probe: an EXTERNAL worker (an ACP coding agent — internal/acp)
// commits with its own git, so the session ledger never sees a git_commit call
// and the gate would read a delivered task as undone (continuation rounds, then
// a deterministic delivery fail). For a setup-provisioned node the clone itself
// is ground truth, so the gate reads it directly. Native workers are unaffected:
// the probe only ever FILLS gaps (committed=false → true), never unsets ledger
// facts.
package vetting

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// augmentFromRepo folds the clone's git state into a session-derived activity:
// commits since the clone's base (baseCommit — the oldest HEAD reflog entry),
// the changed paths (the judge's changed-file re-read + citation grounding),
// the current branch, and — when the task demands a PR/push the worker had no
// stage_pr tool to hand off — a synthesized staged PR from the commits
// themselves, so commitDelivery still posts exactly once, gate-owned.
//
// ponytail: runs a few git subprocesses per activity() call (no caching) — the
// probe fires only on setup-provisioned nodes with an empty ledger; memoise per
// HEAD sha if it ever shows up in a profile.
func augmentFromRepo(act *workerActivity, cfg Config) {
	// A read-only reviewer/explorer makes no commits; synthesizing disk state
	// into a pull_request staging for it would push its (base-HEAD) branch and
	// reset the reviewed PR, wiping commits (#452).
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
	base, err := baseCommit(dir, caps)
	if err != nil {
		return
	}
	head := gitLine(dir, caps, "rev-parse", "HEAD")
	if head == "" || head == base {
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

	// Delivery handoff for a worker with no stage_pr tool: this node is the
	// plan's TERMINAL delivery node (cfg.Deliver non-nil — mid-chain nodes get
	// nil), it is setup-provisioned, and the commits exist — stage the PR from
	// them so the gate delivers exactly once. Deliberately STRUCTURAL, not
	// task-wording-based: a live run whose task text (post-ACP instruction
	// style) never said "open a PR" staged nothing, delivered nothing, and the
	// run reported success anyway.
	if cfg.Deliver != nil {
		if act.stagedDelivery == nil {
			act.stagedDelivery = map[string]StagedDelivery{}
		}
		if _, staged := act.stagedDelivery["pr"]; !staged {
			title := gitLine(dir, caps, "log", "-1", "--format=%s")
			body := strings.Join(gitLines(dir, caps, "log", "--reverse", "--format=- %s", base+".."+head), "\n")
			// Map KEY "pr" (the staging slot hasStagedPR checks); Kind is the
			// DELIVERY discriminator deliverOne switches on — "pull_request",
			// never the slot name (a live delivery failed on kind "pr").
			act.stagedDelivery["pr"] = StagedDelivery{
				Kind:   "pull_request",
				Branch: act.currentBranch,
				Title:  title,
				// Fallback body: the implementer authors a real title+body via the
				// pr-authoring skill and stage_pr (augmentFromPRStage overrides this);
				// this commit-subject synthesis only stands if it never staged one.
				Body: "Commits:\n" + body,
			}
		}
	}
}

// reviewDiffTruncatedMarker caps a review diff at changedFilesBudget (judge.go).
const reviewDiffTruncatedMarker = "\n… (diff truncated)\n"

// buildReviewDiffSection sources a REVIEW node's changedFiles slot from the
// actual PR diff (base..HEAD) off the clone, since act.written is always
// empty for a read-only reviewer (#498 step 1). Best-effort like
// augmentFromRepo: any failure falls back to "" rather than failing the round.
func buildReviewDiffSection(cfg Config) string {
	if cfg.Setup == nil || cfg.Workspace == nil {
		return ""
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil || !isDir(filepath.Join(dir, ".git")) {
		return ""
	}
	caps := checksCaps(cfg)
	base, err := baseCommit(dir, caps)
	if err != nil {
		return ""
	}
	head := gitLine(dir, caps, "rev-parse", "HEAD")
	if head == "" {
		return ""
	}
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "diff", base, head}, caps)
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Output) == "" {
		return ""
	}
	diff := res.Output
	if len(diff) > changedFilesBudget {
		diff = diff[:changedFilesBudget] + reviewDiffTruncatedMarker
	}
	return fmt.Sprintf(
		"DIFF UNDER REVIEW (%s..%s, the actual change this review is OF — verify each finding against this diff, not the review's own description of it):\n\n%s",
		short(base), short(head), diff)
}

// gitLine runs one git command in dir and returns its first output line ("" on
// any failure — the probe is best-effort by construction).
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

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// The disk-truth probe: an EXTERNAL worker (an ACP coding agent - internal/acp)
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

// probeAugmentFromRepo names this probe's execute_tool ledger event
// (emitProbeEvent, probeemit.go) - the disk-truth twin of a real tool call,
// so replay's stream matching has an identity to key on (#604).
const probeAugmentFromRepo = "augment_from_repo"

// augmentFromRepo folds the clone's git state into a session-derived
// activity: commits since the clone's base, changed paths, current branch,
// and - when the task demands delivery but the worker had no stage_pr tool -
// a synthesized staged PR, so commitDelivery still posts exactly once.
//
// ponytail: runs a few git subprocesses per activity() call (no caching) - the
// probe fires only on setup-provisioned nodes with an empty ledger; memoise per
// HEAD sha if it ever shows up in a profile.
//
// ctx carries this call's replay-ledger coordinates - the one execute_tool
// event this emits (emitProbeEvent, deferred below) is what lets a recorded
// ACP round's gate probe replay without a clone. Only the early guard clauses
// skip emission; every path that reaches the clone reports its outcome.
func augmentFromRepo(ctx context.Context, act *workerActivity, cfg Config) {
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

	// Delivery handoff for a worker with no stage_pr tool: this node is the
	// plan's TERMINAL delivery node (cfg.Deliver non-nil - mid-chain nodes get
	// nil), it is setup-provisioned, and the commits exist - stage the PR from
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
			// DELIVERY discriminator deliverOne switches on - "pull_request",
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

// diffTruncatedMarker caps a diff section at changedFilesBudget (judge.go).
const diffTruncatedMarker = "\n… (diff truncated)\n"

// diffSince returns the base...HEAD diff off a setup-provisioned node's clone
// (base = baseCommit, the oldest reflog entry - the commit the work branch
// forked from), truncated to changedFilesBudget, plus the shortened base/head
// SHAs for the caller's header. "" when there's no clone, no commits beyond
// base, or the diff is empty - best-effort like augmentFromRepo, so a git
// failure degrades to no section rather than failing the judge round.
func diffSince(cfg Config) (diff, base, head string) {
	if cfg.Setup == nil || cfg.Workspace == nil {
		return "", "", ""
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(cfg.NodeID))
	if err != nil || !isDir(filepath.Join(dir, ".git")) {
		return "", "", ""
	}
	caps := checksCaps(cfg)
	b, err := baseCommit(dir, caps)
	if err != nil {
		return "", "", ""
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

// buildReviewDiffSection sources a REVIEW node's changedFiles slot from the
// actual PR diff (base...HEAD) off the clone, since act.written is always
// empty for a read-only reviewer (#498 step 1).
func buildReviewDiffSection(cfg Config) string {
	diff, base, head := diffSince(cfg)
	if diff == "" {
		return ""
	}
	return fmt.Sprintf(
		"DIFF UNDER REVIEW (%s..%s, the actual change this review is OF - verify each finding against this diff, not the review's own description of it):\n\n%s",
		base, head, diff)
}

// buildImplementDiffSection sources an IMPLEMENT node's changedFiles slot with
// the actual base...HEAD diff, alongside (not instead of) the full current
// content buildChangedFilesSection already re-reads off disk: several rubric
// criteria (diff_minimality, change_amplification, commit_hygiene,
// deletion_over_addition) judge the SHAPE of the change - what was added vs.
// what already existed - which full file content can't distinguish for an
// edit to a large pre-existing file (#498: "OUTPUT = the staged PR + the diff
// for an implement"). Empty for a node with no Setup clone (a non-code node,
// or an implementer running without a pre-provisioned repo).
func buildImplementDiffSection(cfg Config) string {
	diff, base, head := diffSince(cfg)
	if diff == "" {
		return ""
	}
	return fmt.Sprintf(
		"ACTUAL DIFF THIS NODE PRODUCED (%s..%s - judge what CHANGED here, not just the full file content below: is the diff minimal, does every hunk serve the task, is anything unrelated bundled in):\n\n%s",
		base, head, diff)
}

// gitLine runs one git command in dir and returns its first output line ("" on
// any failure - the probe is best-effort by construction).
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

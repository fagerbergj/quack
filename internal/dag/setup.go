package dag

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupFunc provisions ONE qualifying node's declared clone + branch checkout
// (Plan.Setup) - the harness-executed twin of that node's own git_clone,
// landing at the SAME jail-resolved directory a worker's git_clone would use
// (dir is workspace-relative, ready for jail.Resolve - see
// workspace.SetupCloneDir). Wired in internal/serve over the SAME
// jail/credential/gitTokenSource path the worker git tools use
// (internal/tools.SetupClone).
type SetupFunc func(ctx context.Context, userID, chatID, dir string, setup Setup) error

// SetSetup wires the deterministic setup executor (internal/serve, over
// internal/tools.SetupClone). Unset (nil) means a plan that declares Setup
// hard-errors at run start (runPlanSetup) rather than silently skipping it.
func (e *Executor) SetSetup(fn SetupFunc) { e.setupFn = fn }

// setupQualifyingAgent reports whether an agent can use the plan's ONE
// provisioned clone: the sole WRITER (implementer), which validateRepoChain
// forces into a single depends_on chain since it mutates branch state, plus
// the read-only pair (reviewer, explorer). Neither read-only agent mutates -
// it needs no chain ordering (nothing to corrupt) - but each still needs the
// clone as ground truth, so it gets its OWN linked git worktree off it
// (worktreeParentID) rather than sharing the writer's working tree directly:
// without that, a read-only node's disk reads are unverifiable and its
// citations score 0.00 (see resolveCiteCloneRoots, citationScore's ACP-agent
// case).
func setupQualifyingAgent(name string) bool {
	return name == implementerAgent || name == reviewerAgent || name == explorerAgent
}

// setupQualifyingNodes returns the plan's nodes whose agent can actually use a
// provisioned clone (setupQualifyingAgent). runPlanSetup provisions ONE
// shared clone+branch that the writer resolves into directly (workspace.
// SharedRepoScope) and every read-only qualifying node links its own git
// worktree off (see worktreeParentID) - validateRepoChain (planner.go's
// assemble) guarantees at most one writer runs at a time; read-only nodes
// need no such ordering (their worktrees don't share a working tree at all).
func setupQualifyingNodes(plan Plan) []Node {
	var out []Node
	for _, n := range plan.Nodes {
		if setupQualifyingAgent(n.AgentName) {
			out = append(out, n)
		}
	}
	return out
}

// isReviewOnlySetup reports whether the plan REVIEWS AN EXISTING PR HEAD: a
// reviewerAgent node with no implementerAgent. Node composition alone is NOT
// a reliable signal for Setup.CheckoutExistingHead (#625 - a fix/implement
// plan on an existing PR needs an existing-head checkout too, and always has
// an implementer node); this is used ONLY by OverrideExistingPRHead, to
// decide whether a plan that turns out to have no resolvable PR head ref
// should hard-error (a review has nothing sensible to fall back to) or
// silently proceed as a fresh-branch plan (an issue with no PR yet).
//
// The reviewer is what makes it a review - "no implementer" alone is not
// enough. An explorer-only plan is the plan-only/research shape (read an
// ISSUE's repo to ground a plan), which has no PR and therefore no head ref
// to check out; treating it as review-only made OverrideExistingPRHead demand
// a head ref that an issue never has, and the planner thrashed against the
// error instead of planning.
func isReviewOnlySetup(plan Plan) bool {
	nodes := setupQualifyingNodes(plan)
	if len(nodes) == 0 {
		return false
	}
	hasReviewer := false
	for _, n := range nodes {
		if n.AgentName == reviewerAgent {
			hasReviewer = true
			break
		}
	}
	if !hasReviewer {
		return false
	}
	for _, n := range nodes {
		if n.AgentName == implementerAgent {
			return false
		}
	}
	return true
}

// OverrideExistingPRHead forces Setup.WorkBranch to the run's real PR head
// branch (headRef) and marks Setup.CheckoutExistingHead so the head is
// fetched and checked out as-is rather than branched fresh off BaseRef -
// whenever the run is bound to a REAL existing PR, review OR fix/implement
// alike (#625: node composition alone previously decided this, and any
// implementer node forced checkout -b off base, silently discarding the PR's
// existing commits the moment delivery's necessary --force push landed).
//
// WorkBranch is otherwise planner-authored JSON, and the planner sometimes
// invents a name instead of echoing the real head - a review's
// "quack-auto-review/review-pr-520" (derived from the auto-review commenter
// identity, #520), or a fix/implement plan's own new-branch name (the
// planner is never told a PR head already exists to reuse). Checking out an
// invented name as an existing remote ref fatals with "couldn't find remote
// ref", so it must be overridden, not merely validated.
//
// A review-only plan (isReviewOnlySetup) with no resolvable headRef still
// errors rather than silently keeping the invented name (#520 - there is no
// base-branch fallback that makes sense for a review). Every other
// Setup-bearing plan with no headRef (a plain issue, no PR yet) is left
// untouched - the normal new-branch case. No-op for a plan with no Setup.
func OverrideExistingPRHead(p *Plan, headRef string) error {
	if p == nil || p.Setup == nil {
		return nil
	}
	if headRef == "" {
		if isReviewOnlySetup(*p) {
			return fmt.Errorf("dag: review setup needs the PR's real head branch but none was provided")
		}
		return nil
	}
	p.Setup.WorkBranch = headRef
	p.Setup.CheckoutExistingHead = true
	return nil
}

// runPlanSetup executes the plan's declared PRE-step exactly once, before any
// node runs: clone Setup.Repo at Setup.BaseRef, then either fetch+checkout an
// existing PR head or `checkout -b` a fresh Setup.WorkBranch (Setup.
// CheckoutExistingHead - decided upstream by OverrideExistingPRHead when the
// plan was authored, NOT recomputed here from node composition, #625), into
// ONE shared workspace location (workspace.SetupCloneDir(workspace.
// SharedRepoScope)) - the writer's own working directory (see
// workspaceNodeID). Every read-only qualifying node links its own git
// worktree off THIS clone instead (internal/acp's resolveNode, via Options.
// Worktree - see worktreeParentID), lazily, right before its own round, since
// it may depend on writes this step hasn't made yet. ANY failure - an
// incomplete declaration, a missing setup executor, or the clone/checkout
// itself - aborts the run (a failed run, never a silent no-delivery). A plan
// with no qualifying node is a no-op (nothing will read the clone); a plan
// with plan.Setup == nil is untouched - today's worker-clones behavior.
func (e *Executor) runPlanSetup(ctx context.Context, userID, chatID string, plan Plan) (err error) {
	if plan.Setup == nil {
		return nil
	}
	if len(setupQualifyingNodes(plan)) == 0 {
		return nil
	}
	ctx, span := otelobs.Start(ctx, "setup.clone",
		attribute.String(otelobs.ChatIDKey, chatID), attribute.String("repo", plan.Setup.Repo))
	defer func() { otelobs.End(span, err) }()

	s := *plan.Setup
	if s.Repo == "" || s.BaseRef == "" || s.WorkBranch == "" {
		return fmt.Errorf("dag: setup: repo, base_ref, and work_branch must all be set (got repo=%q base_ref=%q work_branch=%q)",
			s.Repo, s.BaseRef, s.WorkBranch)
	}
	if e.setupFn == nil {
		return fmt.Errorf("dag: plan declares setup but no setup executor is configured")
	}
	dir := workspace.SetupCloneDir(workspace.SharedRepoScope)
	if err = e.setupFn(ctx, userID, chatID, dir, s); err != nil {
		return fmt.Errorf("dag: setup: %w", err)
	}
	return nil
}

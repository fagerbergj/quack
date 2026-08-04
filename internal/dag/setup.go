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
// provisioned clone: the sole WRITER (implementer, forced into one
// depends_on chain by validateRepoChain since it mutates branch state) plus
// the read-only pair (reviewer, explorer). Read-only agents need no chain
// ordering, but each still needs the clone as ground truth, so it gets its
// OWN linked worktree off it (worktreeParentID) rather than sharing the
// writer's tree directly - without that, its disk reads are unverifiable and
// citations score 0.00.
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
// reviewerAgent node with no implementerAgent. Node composition alone isn't
// reliable for CheckoutExistingHead (a fix/implement plan on an existing PR
// also needs an existing-head checkout, and always has an implementer); used
// only by OverrideExistingPRHead to decide hard-error vs falling back to a
// fresh-branch plan when no PR head ref resolves.
//
// The reviewer is what makes it a review - "no implementer" alone isn't
// enough. An explorer-only plan is the research shape (no PR, no head ref),
// and treating it as review-only would make OverrideExistingPRHead demand a
// head ref an issue never has.
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
// branch and marks CheckoutExistingHead, whenever the run is bound to a REAL
// existing PR (review or fix/implement) - node composition alone can't
// decide this, and an implementer node left unforced would checkout -b fresh
// off base, silently discarding the PR's existing commits the moment
// delivery's --force push lands.
//
// WorkBranch is otherwise planner-authored and the planner sometimes invents
// a name instead of echoing the real head; checking out an invented name as
// an existing remote ref fatals with "couldn't find remote ref", so it must
// be overridden, not merely validated.
//
// A review-only plan (isReviewOnlySetup) with no resolvable headRef still
// errors - there's no base-branch fallback that makes sense for a review.
// Every other Setup-bearing plan with no headRef (a plain issue, no PR yet)
// is left untouched. No-op for a plan with no Setup.
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
// node runs: clone Setup.Repo at BaseRef, then either fetch+checkout the
// existing PR head or `checkout -b` a fresh WorkBranch (CheckoutExistingHead
// decided upstream by OverrideExistingPRHead, not recomputed here), into ONE
// shared workspace location - the writer's own working directory. Read-only
// nodes link their own worktree off THIS clone lazily, right before their
// own round, since they may depend on writes this step hasn't made yet. ANY
// failure aborts the run (never a silent no-delivery). A plan with no
// qualifying node, or plan.Setup == nil, is a no-op.
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

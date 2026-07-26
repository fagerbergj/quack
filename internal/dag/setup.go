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
// provisioned clone: the delivery-capable pair (implementer, reviewer),
// which validateRepoChain forces into a single depends_on chain since they
// mutate branch state, plus the read-only explorer. An explorer never
// mutates - it needs no chain ordering (nothing to corrupt) - but it still
// needs the clone as ground truth: without it, its disk reads are
// unverifiable and its citations score 0.00 (see resolveCiteCloneRoots,
// citationScore's ACP-agent case).
func setupQualifyingAgent(name string) bool {
	return name == implementerAgent || name == reviewerAgent || name == explorerAgent
}

// setupQualifyingNodes returns the plan's nodes whose agent can actually use a
// provisioned clone (setupQualifyingAgent). runPlanSetup provisions ONE
// shared clone+branch for the whole set (see workspace.SharedRepoScope) -
// validateRepoChain (planner.go's assemble) guarantees the delivery-capable
// subset forms a single depends_on chain, so sharing is safe; explorers need
// no such ordering (read-only, see setupQualifyingAgent).
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
// reviewerAgent node with no implementerAgent. That is the structural signal
// that Setup.WorkBranch names an EXISTING remote ref to check out rather than
// a new branch to cut off BaseRef.
//
// The reviewer is what makes it a review - "no implementer" alone is not
// enough. An explorer-only plan is the plan-only/research shape (read an
// ISSUE's repo to ground a plan), which has no PR and therefore no head ref
// to check out; treating it as review-only made OverrideReviewWorkBranch
// demand a head ref that an issue never has, and the planner thrashed against
// the error instead of planning.
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

// OverrideReviewWorkBranch forces a review-only plan's Setup.WorkBranch to the
// PR's real head branch (headRef). WorkBranch is otherwise planner-authored
// JSON, and for a review the planner sometimes invents a name instead of
// echoing the real head (e.g. "quack-auto-review/review-pr-520", derived from
// the auto-review commenter identity) - isReviewOnlySetup then checks it out
// as an EXISTING remote ref, which fatals with "couldn't find remote ref" for
// an invented name (#520). No-op for a plan with no Setup or one that isn't
// review-only (the implement path keeps its planner-chosen new-branch name).
// Errors rather than silently keeping the invented name when headRef is
// unknown, so the run fails loudly instead of fetching a bogus ref.
func OverrideReviewWorkBranch(p *Plan, headRef string) error {
	if p == nil || p.Setup == nil || !isReviewOnlySetup(*p) {
		return nil
	}
	if headRef == "" {
		return fmt.Errorf("dag: review setup needs the PR's real head branch but none was provided")
	}
	p.Setup.WorkBranch = headRef
	return nil
}

// runPlanSetup executes the plan's declared PRE-step exactly once, before any
// node runs: clone Setup.Repo at Setup.BaseRef, then checkout -b
// Setup.WorkBranch, into ONE shared workspace location (workspace.
// SetupCloneDir(workspace.SharedRepoScope)) every qualifying node resolves
// into - see dag.workspaceNodeID. ANY failure - an incomplete declaration, a
// missing setup executor, or the clone/checkout itself - aborts the run (a
// failed run, never a silent no-delivery). A plan with no qualifying node is
// a no-op (nothing will read the clone); a plan with plan.Setup == nil is
// untouched - today's worker-clones behavior.
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
	s.CheckoutExistingHead = isReviewOnlySetup(plan)
	if e.setupFn == nil {
		return fmt.Errorf("dag: plan declares setup but no setup executor is configured")
	}
	dir := workspace.SetupCloneDir(workspace.SharedRepoScope)
	if err = e.setupFn(ctx, userID, chatID, dir, s); err != nil {
		return fmt.Errorf("dag: setup: %w", err)
	}
	return nil
}

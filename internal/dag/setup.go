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

// setupQualifyingNodes returns the plan's nodes whose agent can actually use a
// provisioned clone - the delivery-capable agents (implementer, reviewer).
// runPlanSetup provisions ONE shared clone+branch for the whole set (see
// workspace.SharedRepoScope) - validateRepoChain (planner.go's assemble)
// guarantees these nodes form a single depends_on chain, so sharing is safe.
func setupQualifyingNodes(plan Plan) []Node {
	var out []Node
	for _, n := range plan.Nodes {
		if n.AgentName == implementerAgent || n.AgentName == reviewerAgent {
			out = append(out, n)
		}
	}
	return out
}

// isReviewOnlySetup reports whether the plan's qualifying nodes are
// review-only - a reviewerAgent node with NO implementerAgent node. That is
// the structural signal that Setup.WorkBranch names an EXISTING remote PR
// head to check out, not a new branch to create off BaseRef: an implementer
// anywhere in the qualifying set means the chain still needs a fresh branch
// to commit on, even if a reviewer runs later in that same chain.
func isReviewOnlySetup(plan Plan) bool {
	nodes := setupQualifyingNodes(plan)
	if len(nodes) == 0 {
		return false
	}
	for _, n := range nodes {
		if n.AgentName == implementerAgent {
			return false
		}
	}
	return true
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

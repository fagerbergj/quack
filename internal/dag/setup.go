package dag

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupFunc: clone+checkout for plan.Setup.
type SetupFunc func(ctx context.Context, userID, chatID, dir string, setup Setup) error

// SetSetup: wires the setup executor; nil = hard-error at run start.
func (e *Executor) SetSetup(fn SetupFunc) { e.setupFn = fn }

// setupQualifyingAgent: implementer, reviewer, or explorer.
func setupQualifyingAgent(name string) bool {
	return name == implementerAgent || name == reviewerAgent || name == explorerAgent
}

// setupQualifyingNodes: nodes eligible for the provisioned clone.
func setupQualifyingNodes(plan Plan) []Node {
	var out []Node
	for _, n := range plan.Nodes {
		if setupQualifyingAgent(n.AgentName) {
			out = append(out, n)
		}
	}
	return out
}

// isReviewOnlySetup: reviewer without an implementer.
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

// OverrideExistingPRHead: forces Setup.WorkBranch to the real PR head branch.
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

// runPlanSetup: plan's clone+checkout pre-step; failure aborts the run.
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

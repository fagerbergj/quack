package dag

import (
	"context"
	"fmt"
	"strings"

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

// Provision clones+checks out plan.Setup, once. Called eagerly by the
// execute tool (and RunBoundPlan) before the trust-gate run starts, so a
// clone failure surfaces as that caller's own error - a tool-call error the
// orchestrator model can see and revise from, never a run-time abort with a
// raw git dump (#848). Idempotent via Setup.Provisioned, so runPlanSetup
// below never re-clones behind it.
func (e *Executor) Provision(ctx context.Context, userID, chatID string, plan *Plan) (err error) {
	if plan == nil || plan.Setup == nil || plan.Setup.Provisioned {
		return nil
	}
	if len(setupQualifyingNodes(*plan)) == 0 {
		return nil
	}
	s := plan.Setup
	ctx, span := otelobs.Start(ctx, "setup.clone",
		attribute.String(otelobs.ChatIDKey, chatID), attribute.String("repo", s.Repo))
	defer func() { otelobs.End(span, err) }()

	if s.Repo == "" || s.BaseRef == "" || s.WorkBranch == "" {
		return fmt.Errorf("dag: setup: repo, base_ref, and work_branch must all be set (got repo=%q base_ref=%q work_branch=%q)",
			s.Repo, s.BaseRef, s.WorkBranch)
	}
	if e.setupFn == nil {
		return fmt.Errorf("dag: plan declares setup but no setup executor is configured")
	}
	dir := workspace.SetupCloneDir(workspace.SharedRepoScope)
	if cerr := e.setupFn(ctx, userID, chatID, dir, *s); cerr != nil {
		return &setupError{repo: s.Repo, cause: cerr}
	}
	plan.Setup.Provisioned = true
	return nil
}

// runPlanSetup: plan's clone+checkout pre-step; failure aborts the run.
// Delegates to Provision, a no-op if the execute tool (or a bound dispatch)
// already provisioned this plan's Setup.
func (e *Executor) runPlanSetup(ctx context.Context, userID, chatID string, plan Plan) error {
	return e.Provision(ctx, userID, chatID, &plan)
}

// setupError: the human-readable form of a clone failure - what a stream
// error shows (never the raw git argv dump) and what the execute tool
// returns as its tool-call error so the orchestrator model can revise the
// plan instead of dying mid-turn. Unwraps to cause so errors.Is still finds it.
type setupError struct {
	repo  string
	cause error
}

func (e *setupError) Error() string {
	return fmt.Sprintf("plan setup failed: repository %s is unreachable (%s) - revise the plan: "+
		"drop setup if no repository is needed, or name a reachable repository", e.repo, oneLine(e.cause))
}

func (e *setupError) Unwrap() error { return e.cause }

// oneLine collapses a (possibly multi-line) cause to one line, and strips
// runGit's leading "git <argv...>: " dump - the human message should read
// as what git SAID (its stderr), never the invocation that said it.
func oneLine(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if strings.HasPrefix(s, "git ") {
		if _, msg, ok := strings.Cut(s, ": "); ok {
			s = msg
		}
	}
	return s
}

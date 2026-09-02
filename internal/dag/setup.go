package dag

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
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

// staleSetups: chats whose clone the branch has moved under, flagged from
// outside the run (sdk.Host.InvalidateSetup). Package-level because the
// signal arrives on a webhook goroutine that holds no Plan.
var staleSetups sync.Map // chatID -> struct{}

// setupMu serializes the refresh check across all chats: parallel nodes share
// one Plan.Setup, and a slow re-clone holds it. Per-chat locks if that ever
// costs more than the rare boundary check it delays.
var setupMu sync.Mutex

// liveNodes counts a chat's currently-executing gate nodes. Read-only nodes
// work in linked worktrees off the shared clone, so re-cloning it pulls the
// gitdir out from under any sibling still running (#1064).
var liveNodes = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

func enterNode(chatID string) {
	liveNodes.Lock()
	liveNodes.n[chatID]++
	liveNodes.Unlock()
}

func leaveNode(chatID string) {
	liveNodes.Lock()
	liveNodes.n[chatID]--
	if liveNodes.n[chatID] <= 0 {
		delete(liveNodes.n, chatID)
	}
	liveNodes.Unlock()
}

func soleLiveNode(chatID string) bool {
	liveNodes.Lock()
	defer liveNodes.Unlock()
	return liveNodes.n[chatID] == 1
}

// MarkSetupStale records that chatID's clone no longer matches its branch.
// Advisory: the next safe node boundary re-clones, or the run finishes on the
// tree it started with.
func MarkSetupStale(chatID string) { staleSetups.Store(chatID, struct{}{}) }

// clearSetupStale drops the flag. Called at fresh-run start, before setup, so
// a push landing DURING the clone stays flagged.
func clearSetupStale(chatID string) { staleSetups.Delete(chatID) }

// refreshStaleSetup re-clones at a node boundary when the branch moved under
// the run, and reports whether it did. Re-provisioning RemoveAll's the target,
// so it is gated hard: read-only node, review-only plan (an implementer's
// commits live in that tree, unpushed until delivery), no sibling node live in
// a worktree off it, and a clean tree. A stale review beats a run that loses
// its work.
func (e *Executor) refreshStaleSetup(ctx context.Context, userID, chatID string, plan *Plan, node Node, cfg vetting.Config) bool {
	if plan.Setup == nil || !readOnlyQualifyingAgent(node.AgentName) || !isReviewOnlySetup(*plan) {
		return false
	}
	if _, stale := staleSetups.Load(chatID); !stale {
		return false
	}
	setupMu.Lock()
	defer setupMu.Unlock()
	if _, stale := staleSetups.Load(chatID); !stale {
		return false
	}
	if !soleLiveNode(chatID) {
		// Only node entry re-checks, so a fan-out whose siblings never run alone
		// finishes on the stale tree and drops the signal at the next fresh run.
		slog.Info("dag: setup invalidated but a sibling node is running in a worktree off the clone; keeping it, and this run may finish on the stale tree",
			"component", "dag", "chat", chatID, "node", node.ID)
		return false
	}
	if !setupTreeClean(ctx, cfg) {
		slog.Info("dag: setup invalidated but the clone has uncommitted work; keeping it",
			"component", "dag", "chat", chatID, "node", node.ID)
		return false
	}
	plan.Setup.Provisioned = false
	if err := e.Provision(ctx, userID, chatID, plan); err != nil {
		// Flag stays set: a later boundary can retry.
		slog.Warn("dag: re-provisioning the invalidated clone failed",
			"component", "dag", "chat", chatID, "node", node.ID, "err", err)
		return false
	}
	staleSetups.Delete(chatID)
	slog.Info("dag: re-cloned the run's repo at the branch's current head",
		"component", "dag", "chat", chatID, "node", node.ID)
	return true
}

// setupTreeClean: true only when the shared clone exists and has nothing
// uncommitted. Any doubt (unresolvable path, git failure) reads as dirty.
func setupTreeClean(ctx context.Context, cfg vetting.Config) bool {
	if cfg.Workspace == nil {
		return false
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, workspace.SetupCloneDir(workspace.SharedRepoScope))
	if err != nil {
		return false
	}
	res, err := workspace.RunArgv(ctx, dir, []string{"git", "status", "--porcelain"}, cfg.WorkspaceCaps)
	return err == nil && res.ExitCode == 0 && strings.TrimSpace(res.Output) == ""
}

// refreshedNote: told to the worker because an earlier node in this run read
// the pre-push tree; without it the two halves silently disagree.
const refreshedNote = "\n\nNote: the branch was pushed to while this run was in progress. " +
	"The repository has been re-cloned at its current head, so any earlier node's output in this task may describe the previous state."

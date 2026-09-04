package serve

import (
	"context"
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// staleResumePlanCeiling: a paused plan this old is more likely abandoned
// than genuinely mid-run - resuming it would burn a run slot on stale work.
const staleResumePlanCeiling = 24 * time.Hour

// resumeGuardArchivedOrStale is boot resume's cheap admissibility check
// (#1176): an archived chat's paused nodes must never be resumed, and a plan
// older than staleResumePlanCeiling is more likely abandoned than mid-run.
// Split out from the DB reads that feed it so it is unit-testable directly.
func resumeGuardArchivedOrStale(archived, hasPlan bool, planCreatedAt time.Time) (bool, string) {
	if archived {
		return false, "chat archived; not resumed"
	}
	if hasPlan && time.Since(planCreatedAt) > staleResumePlanCeiling {
		return false, "plan older than staleResumePlanCeiling; not resumed"
	}
	return true, ""
}

// reconcileNodes is boot's half of #962: every node the last process left
// suspended is handed back to the scheduler, and the reconcile is logged as
// what actually happened rather than as advice to resend a message.
//
// Runs before the Hub can accept a run (ScanOrphanedRuns is table-wide with no
// liveness check) and returns the nodes worth dispatching; the caller starts
// them once the orchestrator exists.
func reconcileNodes(ctx context.Context, st *store.Store, resumable func(chatID string) (bool, string)) []store.ResumableNode {
	rep, err := st.ResumePausedDagNodes(ctx, resumable)
	if err != nil {
		slog.Error("resume paused dag nodes", "component", "store", "err", err)
		return nil
	}
	// After the reconcile: a hard-kill orphan is `paused` by now, so the chat
	// scan sees it and stamps the chat paused instead of interrupted.
	pausedChats, interrupted, err := st.ScanOrphanedRuns(ctx)
	if err != nil {
		slog.Error("scan orphaned runs", "component", "store", "err", err)
	}
	if n := len(rep.Start); n > 0 {
		slog.Info("resumed paused nodes left by a previous process", "component", "startup",
			"nodes", n, "chats", len(pausedChats))
	}
	if n := len(rep.AwaitingInput); n > 0 {
		slog.Info("chats have nodes awaiting input; answer the question to continue", "component", "startup", "nodes", n)
	}
	for _, f := range rep.Failed {
		slog.Warn("node could not be resumed; marked failed", "component", "startup",
			"plan", f.PlanID, "node", f.NodeID, "reason", f.Reason)
	}
	for _, id := range interrupted {
		slog.Warn("chat left mid-run with no resumable node; marked interrupted - resend the message to retry",
			"component", "startup", "chat", id)
	}
	return rep.Start
}

// startResumedNodes re-enters each resumable node's graph on a server-lifetime
// context, the same shape rest.Handler.startRun uses. One run per chat (the
// Hub registers one run per chat), driving every resumable node of that chat
// in turn - each re-entry is the scoped "node + descendants" subset, so a
// second paused sibling is not covered by the first node's walk.
func startResumedNodes(ctx context.Context, nodes []store.ResumableNode, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub) {
	byChat := map[string][]store.ResumableNode{}
	var order []string
	for _, n := range nodes {
		if len(byChat[n.ChatID]) == 0 {
			order = append(order, n.ChatID)
		}
		byChat[n.ChatID] = append(byChat[n.ChatID], n)
	}
	for _, chatID := range order {
		go driveResume(ctx, chatID, byChat[chatID], orch, st, hub)
	}
}

// driveResume re-enters one chat's resumable nodes. Each node goes through
// the same scoped subset path as a REST retry (RetryNode → runDAGSubset:
// node + descendants, siblings seeded from their stored outputs) - a fresh
// full-plan run would re-execute done nodes.
func driveResume(ctx context.Context, chatID string, nodes []store.ResumableNode, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub) {
	plan, err := st.GetLatestDagPlan(ctx, chatID)
	if err != nil || plan == nil {
		slog.Warn("resume: plan lookup failed", "component", "startup", "chat", chatID, "err", err)
		return
	}
	userID := st.SessionUserForChat(ctx, chatID)
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), 24*time.Hour)
	hub.RegisterRun(chatID, plan.TurnID, cancelRun)
	_ = st.MarkRunActive(runCtx, chatID, plan.TurnID)
	defer hub.Close(chatID)
	defer func() {
		cancelRun()
		hub.UnregisterRun(chatID)
	}()

	eventLog := runlog.NewEventLog(st)
	eventLog.Reset(runCtx, chatID) // old run's (chat_id, seq) rows would PK-collide with the new publisher
	pub := runlog.NewPublisher(hub, eventLog, chatID)
	pub.Publish(stream.ResponseCreated(plan.TurnID))

	var res runlog.DriveResult
	for _, n := range nodes {
		if n.PlanID != plan.ID {
			// A node paused under an older plan isn't in the latest graph;
			// re-entering would silently run zero nodes, forever.
			slog.Warn("resume: node belongs to a non-latest plan, skipping", "component", "startup",
				"chat", chatID, "node", n.NodeID, "plan", n.PlanID, "latest", plan.ID)
			continue
		}
		res = runlog.Drive(plan.TurnID, st, pub, orch.RetryNodeResumed(runCtx, userID, chatID, seededOutputs(runCtx, st, plan.ID), n.NodeID, ""), func(err error) {
			slog.Warn("resume run error", "component", "startup", "chat", chatID, "node", n.NodeID, "err", err)
		})
	}
	pub.Publish(stream.Done())
	runlog.StampTurn(runCtx, st, chatID, plan.TurnID, res)
	st.StampTerminalOutcome(runCtx, orchestrator.AppName, userID, chatID, func() (string, bool) {
		return orch.PendingQuestion(runCtx, userID, chatID)
	})
}

// seededOutputs collects the plan's stored node outputs so a subset re-run
// reads finished siblings instead of re-running them.
func seededOutputs(ctx context.Context, st *store.Store, planID string) map[string]string {
	rows, err := st.GetDagNodes(ctx, planID)
	if err != nil {
		// An empty seed map would make the subset re-run every finished sibling.
		slog.Warn("resume: reading node outputs failed; finished siblings may re-run",
			"component", "startup", "plan", planID, "err", err)
	}
	seeded := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Output != "" {
			seeded[r.NodeID] = r.Output
		}
	}
	return seeded
}

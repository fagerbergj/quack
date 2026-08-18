package serve

import (
	"context"
	"iter"
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

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
// context, the same shape rest.Handler.startRun uses. At most one node per
// chat: a re-entry walks the node's descendants, and the Hub registers one run
// per chat anyway.
func startResumedNodes(ctx context.Context, nodes []store.ResumableNode, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub) {
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n.ChatID] {
			continue
		}
		seen[n.ChatID] = true
		go driveResume(ctx, n, orch, st, hub)
	}
}

func driveResume(ctx context.Context, n store.ResumableNode, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub) {
	plan, err := st.GetLatestDagPlan(ctx, n.ChatID)
	if err != nil || plan == nil {
		slog.Warn("resume: plan lookup failed", "component", "startup", "chat", n.ChatID, "err", err)
		return
	}
	userID := st.SessionUserForChat(ctx, n.ChatID)
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), 24*time.Hour)
	hub.RegisterRun(n.ChatID, plan.TurnID, cancelRun)
	_ = st.MarkRunActive(runCtx, n.ChatID, plan.TurnID)
	defer hub.Close(n.ChatID)
	defer func() {
		cancelRun()
		hub.UnregisterRun(n.ChatID)
	}()

	// No eventLog.Reset: this continues the interrupted run's stream, it does
	// not start a new one.
	pub := runlog.NewPublisher(hub, runlog.NewEventLog(st), n.ChatID)
	run := func(yield func(stream.SSEEvent, error) bool) {
		orch.StartNode(runCtx, userID, n.ChatID, n.NodeID, "", yield)
	}
	res := runlog.Drive(plan.TurnID, st, pub, iter.Seq2[stream.SSEEvent, error](run), func(err error) {
		slog.Warn("resume run error", "component", "startup", "chat", n.ChatID, "node", n.NodeID, "err", err)
	})
	pub.Publish(stream.Done())
	runlog.StampTurn(runCtx, st, n.ChatID, plan.TurnID, res)
	st.StampTerminalOutcome(runCtx, orchestrator.AppName, userID, n.ChatID, func() (string, bool) {
		return orch.PendingQuestion(runCtx, userID, n.ChatID)
	})
}

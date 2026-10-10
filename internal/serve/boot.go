package serve

import (
	"context"
	"iter"
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

// staleResumePlanCeiling: a paused plan this old is more likely abandoned
// than genuinely mid-run - resuming it would burn a run slot on stale work.
const staleResumePlanCeiling = 24 * time.Hour

// bootResumeConcurrency bounds how many chats resume dispatch at once on
// restart, so a boot with many resumable chats doesn't fire them all at once.
const bootResumeConcurrency = 8

// resumeGuardArchivedOrStale: never resume an archived chat or override a human pause. Plans older than
// staleResumePlanCeiling are treated as abandoned, except awaiting_input nodes, whose retry would lose the question.
func resumeGuardArchivedOrStale(archived, hasPlan bool, pauseReason dag.PauseReason, planCreatedAt time.Time) (bool, string) {
	if archived {
		return false, "chat archived; not resumed"
	}
	if pauseReason == dag.PauseUser {
		return false, "paused by a user; not resumed"
	}
	awaitingInput := pauseReason == dag.PauseAwaitingInput
	if !awaitingInput && hasPlan && time.Since(planCreatedAt) > staleResumePlanCeiling {
		return false, "plan older than staleResumePlanCeiling; not resumed"
	}
	return true, ""
}

// reconcileNodes hands every node the last process left suspended back to the scheduler. It runs before the
// Hub accepts runs (ScanOrphanedRuns has no liveness check); the caller starts the returned nodes later.
func reconcileNodes(ctx context.Context, st *store.Store, jail *workspace.Jail, resumable func(chatID, pauseReason string) (bool, string)) []store.ResumableNode {
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
		slog.Warn("chat left mid-run with no resumable node; marked failed - resend the message to retry",
			"component", "startup", "chat", id)
		removeStaleCloneDir(jail, id)
	}
	return rep.Start
}

// syncFinishedNodeRecords repairs recent finished nodes' dag_node records off the boot path;
// older plans are left alone, as boot never resumes them either.
func syncFinishedNodeRecords(ctx context.Context, st *store.Store) {
	n, err := st.SyncTerminalDagNodeRecords(ctx, time.Now().Add(-staleResumePlanCeiling))
	if err != nil {
		slog.Warn("sync finished nodes' dag_node records", "component", "startup", "err", err)
		return
	}
	slog.Info("checked finished nodes' dag_node records against their rows", "component", "startup", "nodes", n)
}

// removeStaleCloneDir clears an interrupted chat's clone so the retry never inherits a read-only Go module
// cache from a killed `go mod download`. Best-effort.
func removeStaleCloneDir(jail *workspace.Jail, chatID string) {
	if jail == nil {
		return
	}
	dir, err := jail.Resolve(localUserID, chatID, workspace.SetupCloneDir(workspace.SharedRepoScope))
	if err != nil {
		return
	}
	if err := workspace.RemoveAllForce(dir); err != nil {
		slog.Warn("remove stale clone dir for interrupted chat failed", "component", "startup", "chat", chatID, "dir", dir, "err", err)
	}
}

// startResumedNodes runs one run per chat (the Hub allows one), re-entering each resumable node's
// node+descendants subset in turn; maxConcurrent caps chats resuming at once.
func startResumedNodes(ctx context.Context, nodes []store.ResumableNode, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, eventLog *runlog.EventLog, maxConcurrent int) {
	byChat := map[string][]store.ResumableNode{}
	var order []string
	for _, n := range nodes {
		if len(byChat[n.ChatID]) == 0 {
			order = append(order, n.ChatID)
		}
		byChat[n.ChatID] = append(byChat[n.ChatID], n)
	}
	// Reset before any resume goroutine starts, so an early subscriber never reads the previous run's events.
	for _, chatID := range order {
		hub.Reset(chatID)
		eventLog.Reset(ctx, chatID)
	}
	boundedGoRun(order, maxConcurrent, func(chatID string) {
		driveResume(ctx, chatID, byChat[chatID], orch, st, hub, eventLog)
	})
}

// boundedGoRun runs run(id) per id, at most maxConcurrent at once. It returns once every goroutine is
// dispatched: the semaphore is acquired inside the goroutine, so boot never blocks on an in-flight run.
func boundedGoRun(ids []string, maxConcurrent int, run func(id string)) {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	sem := make(chan struct{}, maxConcurrent)
	for _, id := range ids {
		go func(id string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			run(id)
		}(id)
	}
}

// driveResume re-enters a chat's resumable nodes via the REST-retry subset path (node + descendants);
// a full-plan run would re-execute done nodes.
func driveResume(ctx context.Context, chatID string, nodes []store.ResumableNode, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, eventLog *runlog.EventLog) {
	plan, err := st.GetLatestDagPlan(ctx, chatID)
	if err != nil || plan == nil {
		slog.Warn("resume: plan lookup failed", "component", "startup", "chat", chatID, "err", err)
		return
	}
	userID := st.SessionUserForChat(ctx, chatID)
	turnID := plan.RunTurnID()
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), 24*time.Hour)
	runCtx = stream.WithTurnID(runCtx, turnID)
	hub.RegisterRun(chatID, turnID, cancelRun)
	_ = st.MarkRunActive(runCtx, chatID, turnID)
	// Reset already ran in startResumedNodes before dispatch; doing it here would race a subscriber.
	defer eventLog.FinishRun(hub, chatID, turnID, cancelRun)

	pub := runlog.NewPublisher(runCtx, hub, eventLog, chatID)
	pub.Publish(stream.ResponseCreated(turnID))

	var res runlog.DriveResult
	for _, n := range nodes {
		if n.PlanID != plan.ID {
			// A node paused under an older plan isn't in the latest graph;
			// re-entering would silently run zero nodes, forever.
			slog.Warn("resume: node belongs to a non-latest plan, skipping", "component", "startup",
				"chat", chatID, "node", n.NodeID, "plan", n.PlanID, "latest", plan.ID)
			failIfNotResumed(runCtx, st, chatID, n.PlanID, n.NodeID, "its plan was superseded")
			continue
		}
		var resumeErr string
		seeded, unreviewed := seededOutputs(runCtx, st, plan.ID)
		run := lastErrorOf(orch.RetryNode(dag.WithUnreviewedSeeds(runCtx, unreviewed), userID, chatID, plan.ID, seeded, n.NodeID, ""), &resumeErr)
		res = runlog.Drive(turnID, st, pub, run, func(err error) {
			slog.Warn("resume run error", "component", "startup", "chat", chatID, "node", n.NodeID, "err", err)
		})
		failIfNotResumed(runCtx, st, chatID, n.PlanID, n.NodeID, resumeErr)
	}
	pub.Publish(stream.Done())
	// A cancel lands on runCtx too, which would fail every write below and leave the chat
	// showing as running forever, so the tail runs on a detached, bounded context.
	tailCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), 10*time.Second)
	defer cancel()
	runlog.StampTurn(tailCtx, st, chatID, turnID, res)
	st.StampTerminalOutcome(tailCtx, orchestrator.AppName, userID, chatID, func() (string, bool) {
		return orch.PendingQuestion(tailCtx, userID, chatID)
	})
}

// lastErrorOf passes run through, keeping the text of its last error event in msg.
func lastErrorOf(run iter.Seq2[stream.SSEEvent, error], msg *string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		for ev, err := range run {
			if d, ok := ev.Data.(stream.ErrorData); ok {
				*msg = d.Error
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// failIfNotResumed settles a node the resume errored out on before it moved (e.g. no
// plan in session): left paused/shutdown, every later boot would retry it again.
func failIfNotResumed(ctx context.Context, st *store.Store, chatID, planID, nodeID, why string) {
	if why == "" || ctx.Err() != nil {
		return
	}
	n, err := st.GetDagNode(ctx, planID, nodeID)
	if err != nil || n == nil || n.Status != string(dag.StatusPaused) || n.PauseReason != string(dag.PauseShutdown) {
		return
	}
	st.FailUnresumable(ctx, chatID, *n, why)
}

// seededOutputs collects the plan's stored node outputs so a subset re-run
// reads finished siblings instead of re-running them.
func seededOutputs(ctx context.Context, st *store.Store, planID string) (map[string]string, map[string]bool) {
	rows, err := st.GetDagNodes(ctx, planID)
	if err != nil {
		// An empty seed map would make the subset re-run every finished sibling.
		slog.Warn("resume: reading node outputs failed; finished siblings may re-run",
			"component", "startup", "plan", planID, "err", err)
	}
	return store.SeedOutputs(rows)
}

package serve

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"time"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"

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

// resumeGuardArchivedOrStale: never resume an archived chat. Plans older than staleResumePlanCeiling are treated
// as abandoned, except for nodes waiting on a person: an answer, or a user pause the store holds paused.
func resumeGuardArchivedOrStale(archived, hasPlan bool, pauseReason dag.PauseReason, planCreatedAt time.Time) (bool, string) {
	if archived {
		return false, "chat archived; not resumed"
	}
	waiting := pauseReason == dag.PauseAwaitingInput || pauseReason == dag.PauseUser
	if !waiting && hasPlan && time.Since(planCreatedAt) > staleResumePlanCeiling {
		return false, "plan older than staleResumePlanCeiling; not resumed"
	}
	return true, ""
}

// bootResume is what the reconcile left for the started server: nodes to re-enter, and chats it ended instead.
type bootResume struct {
	Start   []store.ResumableNode
	Settled []settledRun // left mid-run, now failed with nothing to resume
	Finish  []string     // chats cut after every node was done, owed only their delivery
}

// settledRun: PlanRan is false for a turn interrupted before it planned, so no node row exists.
type settledRun struct {
	ChatID  string
	PlanRan bool
}

// reconcileNodes hands every node the last process left suspended back to the scheduler. It runs before the
// Hub accepts runs (ScanOrphanedRuns has no liveness check); the caller starts the returned nodes later.
func reconcileNodes(ctx context.Context, st *store.Store, jail *workspace.Jail, resumable func(chatID, pauseReason string) (bool, string)) bootResume {
	rep, err := st.ResumePausedDagNodes(ctx, resumable)
	if err != nil {
		slog.Error("resume paused dag nodes", "component", "store", "err", err)
		return bootResume{}
	}
	// After the reconcile: a hard-kill orphan is `paused` by now, so the chat
	// scan sees it and stamps the chat paused instead of interrupted.
	pausedChats, interrupted, finishable, err := st.ScanOrphanedRuns(ctx)
	if err != nil {
		slog.Error("scan orphaned runs", "component", "store", "err", err)
	}
	if n := len(rep.Start); n > 0 {
		slog.Info("resumed paused nodes left by a previous process", "component", "startup",
			"nodes", n, "chats", len(pausedChats))
	}
	if n := len(rep.Held); n > 0 {
		slog.Info("nodes a user paused stay paused; start them to continue", "component", "startup", "nodes", n)
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
	return bootResume{Start: rep.Start, Settled: settledChats(rep, interrupted), Finish: finishable}
}

// settledChats: chats with a failed node or an interrupted turn and nothing resumed or awaiting an answer.
func settledChats(rep store.ResumeReport, interrupted []string) []settledRun {
	live := map[string]bool{}
	for _, n := range slices.Concat(rep.Start, rep.AwaitingInput, rep.Held) {
		live[n.ChatID] = true
	}
	seen := map[string]bool{}
	var out []settledRun
	add := func(id string, planRan bool) {
		if id != "" && !live[id] && !seen[id] {
			seen[id] = true
			out = append(out, settledRun{ChatID: id, PlanRan: planRan})
		}
	}
	for _, f := range rep.Failed {
		add(f.ChatID, true)
	}
	for _, id := range interrupted {
		add(id, false)
	}
	return out
}

// endSettledExtRuns tells each dispatching extension that its run ended at boot, so it can post the failure
// and release the session instead of holding it until its lease expires.
func endSettledExtRuns(runs []settledRun, exts []builtSDKExtension) {
	for _, r := range runs {
		if obs := runObserverFor(r.ChatID, exts); obs != nil {
			endSettledExtRun(obs, r)
		}
	}
}

// endSettledExtRun isolates extension code: a panic must not crash boot or skip the remaining chats.
func endSettledExtRun(obs extsdk.RunObserver, run settledRun) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("boot: extension RunEnded panicked", "component", "startup", "chat", run.ChatID, "err", r)
		}
	}()
	slog.Info("boot ended an extension run it could not resume", "component", "startup", "chat", run.ChatID)
	obs.RunEnded(run.ChatID, extsdk.RunOutcome{Status: extsdk.RunFailed, PlanRan: run.PlanRan, Error: settledRunError})
}

const settledRunError = "quack restarted and could not resume this run; re-trigger it to start over"

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

// startResumedNodes runs one run per chat (the Hub allows one), re-entering all of a chat's resumable nodes
// and their descendants together; maxConcurrent caps chats resuming at once.
func startResumedNodes(ctx context.Context, nodes []store.ResumableNode, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, eventLog *runlog.EventLog, exts []builtSDKExtension, maxConcurrent int) {
	byChat := map[string][]store.ResumableNode{}
	var order []string
	for _, n := range nodes {
		if len(byChat[n.ChatID]) == 0 {
			order = append(order, n.ChatID)
		}
		byChat[n.ChatID] = append(byChat[n.ChatID], n)
	}
	// Before any resume goroutine starts, so an early subscriber reads the whole transcript, old events first.
	for _, chatID := range order {
		hub.Reset(chatID)
		eventLog.Reopen(ctx, hub, chatID)
	}
	boundedGoRun(order, maxConcurrent, func(chatID string) {
		driveResume(ctx, chatID, byChat[chatID], orch, st, hub, eventLog, runObserverFor(chatID, exts))
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

// driveResume re-enters a chat's resumable nodes in one subset run (nodes + descendants), so a descendant
// they share runs once; a full-plan run would re-execute done nodes.
// obs, when non-nil, is the dispatching extension: it gets RunEnded exactly as an uninterrupted run would.
func driveResume(ctx context.Context, chatID string, nodes []store.ResumableNode, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, eventLog *runlog.EventLog, obs extsdk.RunObserver) {
	driveBootRun(ctx, chatID, orch, st, hub, eventLog, obs, func(runCtx context.Context, plan *store.DagPlan, userID string, pub *runlog.Publisher) runlog.DriveResult {
		var ids []string
		for _, n := range nodes {
			if n.PlanID != plan.ID {
				// A node paused under an older plan isn't in the latest graph;
				// re-entering would silently run zero nodes, forever.
				slog.Warn("resume: node belongs to a non-latest plan, skipping", "component", "startup",
					"chat", chatID, "node", n.NodeID, "plan", n.PlanID, "latest", plan.ID)
				failIfNotResumed(runCtx, st, chatID, n.PlanID, n.NodeID, "its plan was superseded")
				continue
			}
			ids = append(ids, n.NodeID)
		}
		if len(ids) == 0 {
			return runlog.DriveResult{}
		}
		var resumeErr string
		seeded, unreviewed := seededOutputs(runCtx, st, plan.ID)
		run := lastErrorOf(orch.ResumeNodes(dag.WithUnreviewedSeeds(runCtx, unreviewed), userID, chatID, plan.ID, seeded, ids), &resumeErr)
		res := runlog.Drive(plan.RunTurnID(), st, pub, run, func(err error) {
			slog.Warn("resume run error", "component", "startup", "chat", chatID, "nodes", ids, "err", err)
		})
		for _, id := range ids {
			failIfNotResumed(runCtx, st, chatID, plan.ID, id, resumeErr)
		}
		return res
	})
}

// finishCutRuns finishes each chat boot found cut after its nodes were done. It runs after the extensions
// start, as endSettledExtRuns does: RunEnded needs their restored claims.
func finishCutRuns(ctx context.Context, chatIDs []string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, eventLog *runlog.EventLog, exts []builtSDKExtension) {
	for _, chatID := range chatIDs {
		hub.Reset(chatID)
		eventLog.Reopen(ctx, hub, chatID)
		finishCutRun(ctx, chatID, orch, st, hub, eventLog, runObserverFor(chatID, exts))
	}
}

// finishCutRun ends a run a restart cut after every node finished: it delivers the sinks' answer and runs the
// usual run tail, so the chat ends done and its extension hears the real outcome.
func finishCutRun(ctx context.Context, chatID string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, eventLog *runlog.EventLog, obs extsdk.RunObserver) {
	driveBootRun(ctx, chatID, orch, st, hub, eventLog, obs, func(runCtx context.Context, plan *store.DagPlan, userID string, _ *runlog.Publisher) runlog.DriveResult {
		execPlan, ok := st.LoadExecPlan(runCtx, plan.ID)
		if !ok {
			return runlog.DriveResult{}
		}
		seeded, _ := seededOutputs(runCtx, st, plan.ID)
		orch.FinishCutRun(runCtx, userID, chatID, execPlan, seeded)
		slog.Info("finished a run cut after its nodes were done", "component", "startup", "chat", chatID)
		return runlog.DriveResult{PlanID: plan.ID}
	})
}

// driveBootRun runs body as the chat's run on its latest plan's turn, then the run tail: done, the turn stamp,
// and RunEnded to obs (or the terminal outcome stamp).
func driveBootRun(ctx context.Context, chatID string, orch *orchestrator.Orchestrator, st *store.Store, hub *stream.Hub, eventLog *runlog.EventLog, obs extsdk.RunObserver,
	body func(runCtx context.Context, plan *store.DagPlan, userID string, pub *runlog.Publisher) runlog.DriveResult) {
	plan, err := st.GetLatestDagPlan(ctx, chatID)
	if err != nil || plan == nil {
		// The chat keeps its paused nodes or in-flight marker, so the next boot retries it.
		slog.Warn("resume: plan lookup failed; left for the next boot", "component", "startup", "chat", chatID, "err", err)
		endReopenedStream(ctx, hub, eventLog, chatID)
		return
	}
	userID := st.SessionUserForChat(ctx, chatID)
	turnID := plan.RunTurnID()
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(ctx), 24*time.Hour)
	runCtx = stream.WithTurnID(runCtx, turnID)
	if !hub.RegisterRun(chatID, turnID, cancelRun) {
		// A dispatch got the chat first and runs a newer turn; a node it leaves paused resumes next boot.
		cancelRun()
		slog.Warn("resume: the chat already has a run; not resuming", "component", "startup", "chat", chatID)
		return
	}
	_ = st.MarkRunActive(runCtx, chatID, turnID)
	// Reopen already ran in startResumedNodes before dispatch; doing it here would race a subscriber.
	defer eventLog.FinishRun(hub, chatID, turnID, cancelRun)

	pub := runlog.NewPublisher(runCtx, hub, eventLog, chatID)
	pub.Publish(stream.ResponseCreated(turnID))
	res := body(runCtx, plan, userID, pub)
	pub.Publish(stream.Done())
	// A cancel lands on runCtx too, which would fail every write below and leave the chat
	// showing as running forever, so the tail runs on a detached, bounded context.
	tailCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), 10*time.Second)
	defer cancel()
	runlog.StampTurn(tailCtx, st, chatID, turnID, res)
	// A run cut by another shutdown leaves the chat active, with no RunEnded: the next boot resumes or finishes it.
	if hub.WasInterrupted(chatID) {
		return
	}
	if obs != nil {
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		cancelled := errors.Is(runCtx.Err(), context.Canceled)
		out := buildExtRunOutcome(tailCtx, orch, st, userID, chatID, res.PlanID != "", res.NeedsInput, timedOut, cancelled)
		slog.Info("resume: run ended; outcome delivered to its extension", "component", "startup", "chat", chatID, "status", out.Status)
		eventLog.EndingRun(hub, chatID, turnID)
		obs.RunEnded(chatID, out)
		return
	}
	st.StampTerminalOutcome(tailCtx, orchestrator.AppName, userID, chatID, func() (string, bool) {
		return orch.PendingQuestion(tailCtx, userID, chatID)
	})
}

// endReopenedStream ends the transcript Reopen replayed for a boot run that never started, so no subscriber
// waits on it forever. A run already holding the chat owns the stream and closes it itself.
func endReopenedStream(ctx context.Context, hub *stream.Hub, eventLog *runlog.EventLog, chatID string) {
	noop := func() {}
	if !hub.RegisterRun(chatID, "", noop) {
		return
	}
	runlog.NewPublisher(ctx, hub, eventLog, chatID).Publish(stream.Done())
	eventLog.FinishRun(hub, chatID, "", noop)
}

// runObserverFor returns the extension that dispatched chatID ("ext:<name>:..."), if it observes runs.
func runObserverFor(chatID string, exts []builtSDKExtension) extsdk.RunObserver {
	rest, ok := strings.CutPrefix(chatID, "ext:")
	if !ok {
		return nil
	}
	name, _, _ := strings.Cut(rest, ":")
	for _, e := range exts {
		if obs, ok := e.ext.(extsdk.RunObserver); ok && e.name == name {
			return obs
		}
	}
	return nil
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

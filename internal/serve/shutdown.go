package serve

import (
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// settleWindow: fixed grace-on-top-of-grace for a force-cancelled run's own
// goroutine to unwind and persist before the process exits - not a knob
// operators need to tune.
const settleWindow = 5 * time.Second

const drainPollInterval = 200 * time.Millisecond

// nodePauser is the executor seam the drain needs: which nodes are live on a
// chat, and how to pause one. *dag.Executor implements it.
type nodePauser interface {
	ActiveNodes(chatID string) []string
	PauseNode(chatID, nodeID string, reason dag.PauseReason) bool
}

// DrainActiveRuns is SIGTERM's counterpart to store.ScanOrphanedRuns: reject
// new dispatches, pause every running node with reason=shutdown, and give the
// runs up to grace to reach a gate boundary. A node still mid-turn at the
// deadline is already persisted `paused/shutdown` (PauseNode's store write is
// synchronous), so cancelling its in-flight turn loses only the turn - boot
// resumes the node from its pause. Nothing here marks a chat interrupted:
// interrupted means "cut short, resend to resume", and a shutdown pause is
// resumed by the server itself (#962).
func DrainActiveRuns(hub *stream.Hub, ex nodePauser, grace time.Duration) {
	hub.BeginDraining()
	ids := hub.ActiveChatIDs()
	var paused, chats int
	if ex != nil {
		for _, chatID := range ids {
			n := 0
			for _, nodeID := range ex.ActiveNodes(chatID) {
				if ex.PauseNode(chatID, nodeID, dag.PauseShutdown) {
					n++
				}
			}
			if n > 0 {
				paused += n
				chats++
			}
		}
	}
	if len(ids) > 0 {
		slog.Info("paused running nodes for shutdown", "component", "serve",
			"nodes", paused, "chats", chats, "grace", grace)
	}

	// Re-reads hub.ActiveChatIDs() on every poll rather than iterating the
	// snapshot above: a dispatch that checked Draining()==false just before
	// BeginDraining flipped it registers moments later, for a chat this
	// snapshot never saw - iterating the frozen ids would make that run
	// permanently invisible to drain instead of merely late.
	waitWhileAnyRegistered(hub, grace)

	remaining := hub.ActiveChatIDs()
	for _, chatID := range remaining {
		hub.MarkInterrupted(chatID) // per-chat cut marker: only force-cancelled runs skip their RunEnded tail
		hub.CancelRun(chatID)
	}
	if len(remaining) == 0 {
		return
	}
	slog.Warn("cancelled in-flight turns past the shutdown grace window; their nodes stay paused/shutdown and resume at boot",
		"component", "serve", "count", len(remaining))
	waitWhileAnyRegistered(hub, settleWindow)
}

// waitWhileAnyRegistered polls hub.ActiveChatIDs() until it is empty, or
// budget elapses.
func waitWhileAnyRegistered(hub *stream.Hub, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if len(hub.ActiveChatIDs()) == 0 {
			return
		}
		time.Sleep(drainPollInterval)
	}
}

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
	if len(ids) == 0 {
		return
	}
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
	slog.Info("paused running nodes for shutdown", "component", "serve",
		"nodes", paused, "chats", chats, "grace", grace)

	waitWhileAnyRegistered(hub, ids, grace)

	var forced int
	for _, chatID := range ids {
		if !hub.HasRegisteredRun(chatID) {
			continue // reached a boundary on its own within the grace window
		}
		forced++
		hub.CancelRun(chatID)
	}
	if forced == 0 {
		return
	}
	slog.Warn("cancelled in-flight turns past the shutdown grace window; their nodes stay paused/shutdown and resume at boot",
		"component", "serve", "count", forced)
	waitWhileAnyRegistered(hub, ids, settleWindow)
}

// waitWhileAnyRegistered polls until none of ids has a registered run, or
// budget elapses.
func waitWhileAnyRegistered(hub *stream.Hub, ids []string, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		none := true
		for _, id := range ids {
			if hub.HasRegisteredRun(id) {
				none = false
				break
			}
		}
		if none {
			return
		}
		time.Sleep(drainPollInterval)
	}
}

package serve

import (
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// settleWindow: extra grace for a force-cancelled run to unwind and persist before exit; not a knob.
const settleWindow = 5 * time.Second

const drainPollInterval = 200 * time.Millisecond

// nodePauser is the executor seam the drain needs: which nodes are live on a
// chat, and how to pause one. *dag.Executor implements it.
type nodePauser interface {
	ActiveNodes(chatID string) []string
	PauseNode(chatID, nodeID string, reason dag.PauseReason) bool
	MarkShutdown(chatID string)
}

// DrainActiveRuns rejects new dispatches, pauses running nodes (reason=shutdown) and waits up to grace.
// The pause is persisted synchronously, so cancelling a mid-turn node loses only the turn; boot resumes it.
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
		// nodes counts live gate controls only; a node still in admission or setup has none yet.
		slog.Info("paused live nodes for shutdown; runs with none are cancelled after the grace", "component", "serve",
			"nodes", paused, "chats", chats, "runs", len(ids), "grace", grace)
	}

	// Re-read ActiveChatIDs each poll: a dispatch that saw Draining()==false just before the flip registers
	// later, for a chat the snapshot above never saw.
	waitWhileAnyRegistered(hub, grace)

	remaining := hub.ActiveChatIDs()
	for _, chatID := range remaining {
		hub.MarkInterrupted(chatID) // per-chat cut marker: only force-cancelled runs skip their RunEnded tail
		if ex != nil {
			// A node that registered after the sweep above is unpaused; this keeps its abort from settling it cancelled.
			ex.MarkShutdown(chatID)
		}
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

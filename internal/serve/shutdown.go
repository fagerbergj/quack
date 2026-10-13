package serve

import (
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/stream"
)

// settleWindow: extra grace for a force-cancelled run to unwind and persist before exit; not a knob.
const settleWindow = 5 * time.Second

const drainPollInterval = 200 * time.Millisecond

// nodePauser is the executor seam the drain needs: which nodes are live on a
// chat, and how to pause one. *dag.Executor implements it.
type nodePauser interface {
	ActiveNodes(chatID string) []string
	PauseNodeForShutdown(chatID, nodeID string, mark func(chatID string)) bool
	MarkShutdown(chatID string)
}

// DrainActiveRuns rejects new dispatches, pauses running nodes (reason=shutdown) at once and waits up to grace.
// The pause is persisted synchronously and keeps the node's session, so boot continues it where it was cut.
func DrainActiveRuns(hub *stream.Hub, ex nodePauser, grace time.Duration) {
	hub.BeginDraining()
	ids := hub.ActiveChatIDs()
	var paused, chats int
	if ex != nil {
		for _, chatID := range ids {
			n := pauseForShutdown(hub, ex, chatID)
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
		hub.MarkInterrupted(chatID) // per-chat cut marker: a cut run skips its RunEnded tail
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

// pauseForShutdown pauses chatID's live nodes, cutting off their worker rounds. A pause marks the chat interrupted
// atomically with finding the node live: its run now ends inside the grace, and its tail must skip RunEnded.
func pauseForShutdown(hub *stream.Hub, ex nodePauser, chatID string) int {
	n := 0
	for _, nodeID := range ex.ActiveNodes(chatID) {
		if ex.PauseNodeForShutdown(chatID, nodeID, hub.MarkInterrupted) {
			n++
		}
	}
	return n
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

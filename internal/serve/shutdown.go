package serve

import (
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/stream"
)

// settleWindow: fixed grace-on-top-of-grace for a force-cancelled run's own
// goroutine to unwind and persist before the process exits - not a knob
// operators need to tune.
const settleWindow = 5 * time.Second

const drainPollInterval = 200 * time.Millisecond

// DrainActiveRuns is SIGTERM's counterpart to store.ScanOrphanedRuns: reject
// new dispatches, give in-flight runs up to grace, then force-cancel and
// MarkInterrupted whatever's left so the run's own tail skips RunEnded.
func DrainActiveRuns(hub *stream.Hub, grace time.Duration) {
	hub.BeginDraining()
	ids := hub.ActiveChatIDs()
	if len(ids) == 0 {
		return
	}
	slog.Info("draining active runs before shutdown", "component", "serve", "count", len(ids), "grace", grace)

	waitWhileAnyRegistered(hub, ids, grace)

	var forced int
	for _, chatID := range ids {
		if !hub.HasRegisteredRun(chatID) {
			continue // finished on its own within the grace window
		}
		forced++
		hub.MarkInterrupted(chatID)
		hub.CancelRun(chatID)
	}
	if forced == 0 {
		return
	}
	slog.Warn("force-cancelled runs still active past the shutdown grace window", "component", "serve", "count", forced)
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

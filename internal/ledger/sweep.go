package ledger

import (
	"context"
	"log/slog"
	"time"
)

// RunRetentionSweep deletes whole recorded sessions last modified before
// retentionDays ago - once immediately, then every tick, until ctx is done.
// A nil store or retentionDays <= 0 (config's "0 = forever") is a no-op: it
// returns immediately without starting the ticker. GC only ever drops a
// whole session file, never edits one (see LedgerStore.Delete).
func RunRetentionSweep(ctx context.Context, store LedgerStore, retentionDays int, tick time.Duration) {
	if store == nil || retentionDays <= 0 {
		return
	}
	sweepOnce(ctx, store, retentionDays)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepOnce(ctx, store, retentionDays)
		}
	}
}

func sweepOnce(ctx context.Context, store LedgerStore, retentionDays int) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	sessions, err := store.List(ctx)
	if err != nil {
		slog.Warn("replay ledger retention sweep: list failed", "component", "ledger", "err", err)
		return
	}
	for _, s := range sessions {
		if s.ModTime.After(cutoff) {
			continue
		}
		if err := store.Delete(ctx, s.ID); err != nil {
			slog.Warn("replay ledger retention sweep: delete failed", "component", "ledger", "session", s.ID, "err", err)
			continue
		}
		slog.Info("replay ledger retention sweep: deleted expired session", "component", "ledger", "session", s.ID, "mod_time", s.ModTime)
	}
}

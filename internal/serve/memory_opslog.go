package serve

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/store"
)

// storeOpsLog adapts internal/store to memory.OpsLog here because internal/memory can't import
// internal/store (the dependency runs the other way).
type storeOpsLog struct{ st *store.Store }

func (o storeOpsLog) LogMemoryOp(ctx context.Context, memoryID string, op memory.OpsLogOp, actor memory.OpsLogActor, reason string) error {
	return o.st.InsertMemoryOp(ctx, memoryID, string(op), string(actor), reason)
}

func (o storeOpsLog) PruneMemoryOps(ctx context.Context, cutoff time.Time) (int, error) {
	return o.st.PruneMemoryOps(ctx, cutoff)
}

// startConsolidationSweep launches s's cron-scheduled burst-dedupe +
// retention job. Validate() always fills Schedule, so it's never nil here.
func startConsolidationSweep(ctx context.Context, s *memory.Store, rm config.ResolvedMemory, loc *time.Location) {
	schedule := *rm.Consolidation.Schedule
	if schedule == "" {
		return
	}
	go s.RunConsolidationSweep(ctx, zonedSchedule(schedule, loc), rm.Consolidation.RetentionDays)
}

// zonedSchedule runs a cron in the configured zone unless it names its own.
func zonedSchedule(schedule string, loc *time.Location) string {
	if strings.HasPrefix(schedule, "CRON_TZ=") || strings.HasPrefix(schedule, "TZ=") {
		return schedule
	}
	return "CRON_TZ=" + loc.String() + " " + schedule
}

// logTimezone reports the zone agents see; Go silently falls back to UTC on an unset or unknown TZ.
func logTimezone(cfg *config.Config) {
	abbr, _ := time.Now().In(cfg.Location()).Zone()
	if cfg.Timezone != "" {
		slog.Info("time zone", "component", "startup", "zone", cfg.Timezone, "abbr", abbr)
		return
	}
	tz := os.Getenv("TZ")
	if _, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); tz != "" && err != nil {
		slog.Warn("TZ is not a known zone, so agents see UTC; set timezone (QUACK_TIMEZONE)", "component", "startup", "TZ", tz)
		return
	}
	if time.Local.String() == "UTC" || abbr == "UTC" {
		slog.Warn("no time zone configured, so agents see UTC; set timezone (QUACK_TIMEZONE)", "component", "startup")
		return
	}
	slog.Info("time zone", "component", "startup", "zone", "server local", "abbr", abbr)
}

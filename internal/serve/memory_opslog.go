package serve

import (
	"context"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/store"
)

// storeOpsLog adapts internal/store's persistence to memory.OpsLog. This is
// the one place both packages meet: internal/memory can't import
// internal/store (dependency direction runs the other way), so bootstrap
// wires the concrete audit sink here (see openMemory in serve.go).
type storeOpsLog struct{ st *store.Store }

func (o storeOpsLog) LogMemoryOp(ctx context.Context, memoryID string, op memory.OpsLogOp, actor memory.OpsLogActor, reason string) error {
	return o.st.InsertMemoryOp(ctx, memoryID, string(op), string(actor), reason)
}

func (o storeOpsLog) PruneMemoryOps(ctx context.Context, cutoff time.Time) (int, error) {
	return o.st.PruneMemoryOps(ctx, cutoff)
}

// startConsolidationSweep launches s's cron-scheduled burst-dedupe +
// retention job. Validate() always fills Schedule, so it's never nil here.
func startConsolidationSweep(ctx context.Context, s *memory.Store, rm config.ResolvedMemory) {
	schedule := *rm.Consolidation.Schedule
	if schedule == "" {
		return
	}
	go s.RunConsolidationSweep(ctx, schedule, rm.Consolidation.RetentionDays)
}

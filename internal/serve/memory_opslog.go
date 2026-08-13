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

// startConsolidationSweep launches s's periodic burst-dedupe + retention job
// (design doc §4(c)/§6, memory.Store.RunConsolidationSweep) next to
// ledger.RunRetentionSweep's own bootstrap call. Gated on interval > 0 -
// otherwise nothing starts, matching the ledger sweep's disable convention.
func startConsolidationSweep(ctx context.Context, s *memory.Store, rm config.ResolvedMemory) {
	if rm.Consolidation.IntervalMinutes <= 0 {
		return
	}
	interval := time.Duration(rm.Consolidation.IntervalMinutes) * time.Minute
	go s.RunConsolidationSweep(ctx, interval, rm.Consolidation.RetentionDays)
}

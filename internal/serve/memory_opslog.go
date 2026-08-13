package serve

import (
	"context"

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

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MemoryOp is one append-only audit row for a memory lifecycle transition
// (design doc docs/memory-lifecycle.md §3, issue #849 phase 2). Rows are
// never updated or deleted by this package - a memory's own soft-delete
// leaves its history intact.
type MemoryOp struct {
	ID        string `gorm:"primaryKey"`
	MemoryID  string `gorm:"index"`
	Op        string // add|update|delete|reinforce|invalidate
	Actor     string // consolidator|outcome-feedback|human|run
	Reason    string `gorm:"type:text"`
	Timestamp time.Time
}

func (MemoryOp) TableName() string { return "memory_ops" }

// InsertMemoryOp appends one audit row. Implements memory.OpsLog's backing
// call - internal/memory can't import internal/store, so internal/serve
// wires an adapter around this method at bootstrap (see Store.SetOpsLog).
func (s *Store) InsertMemoryOp(ctx context.Context, memoryID, op, actor, reason string) error {
	row := &MemoryOp{ID: uuid.NewString(), MemoryID: memoryID, Op: op, Actor: actor, Reason: reason, Timestamp: time.Now().UTC()}
	return s.db.WithContext(ctx).Create(row).Error
}

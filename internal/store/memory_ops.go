package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MemoryOp is one append-only audit row for a memory lifecycle transition (docs/memory-lifecycle.md).
// Never updated or deleted here, so a memory's soft-delete keeps its history.
type MemoryOp struct {
	ID        string `gorm:"primaryKey"`
	MemoryID  string `gorm:"index"`
	Op        string // add|update|delete|reinforce|invalidate
	Actor     string // consolidator|outcome-feedback|human|run
	Reason    string `gorm:"type:text"`
	Timestamp time.Time
}

func (MemoryOp) TableName() string { return "memory_ops" }

// InsertMemoryOp appends one audit row; internal/serve adapts it into memory.OpsLog (memory can't import store).
func (s *Store) InsertMemoryOp(ctx context.Context, memoryID, op, actor, reason string) error {
	row := &MemoryOp{ID: uuid.NewString(), MemoryID: memoryID, Op: op, Actor: actor, Reason: reason, Timestamp: time.Now().UTC()}
	return s.db.WithContext(ctx).Create(row).Error
}

// PruneMemoryOps hard-deletes memory_ops rows older than cutoff. Implements
// memory.OpsLog's prune call (see internal/serve's storeOpsLog adapter).
func (s *Store) PruneMemoryOps(ctx context.Context, cutoff time.Time) (int, error) {
	res := s.db.WithContext(ctx).Where("timestamp < ?", cutoff).Delete(&MemoryOp{})
	if res.Error != nil {
		return 0, res.Error
	}
	return int(res.RowsAffected), nil
}

// ListMemoryOps returns every memory_ops row at or after since, oldest first, for `quack memory stats`.
// One audit table spans every memory backend.
func (s *Store) ListMemoryOps(ctx context.Context, since time.Time) ([]MemoryOp, error) {
	var rows []MemoryOp
	if err := s.db.WithContext(ctx).Where("timestamp >= ?", since).Order("timestamp ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

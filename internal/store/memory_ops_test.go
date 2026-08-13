package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestInsertMemoryOp pins the memory_ops audit table (design doc §3, issue
// #849 phase 2): AutoMigrate creates it on the same sqlite path every other
// app table uses, and rows are append-only (two rows for the same memory_id
// both persist, not upserted).
func TestInsertMemoryOp(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()

	if err := st.InsertMemoryOp(ctx, "mem-1", "add", "consolidator", ""); err != nil {
		t.Fatalf("InsertMemoryOp (add): %v", err)
	}
	if err := st.InsertMemoryOp(ctx, "mem-1", "invalidate", "outcome-feedback", "pr closed unmerged"); err != nil {
		t.Fatalf("InsertMemoryOp (invalidate): %v", err)
	}

	var rows []MemoryOp
	if err := st.db.WithContext(ctx).Where("memory_id = ?", "mem-1").Order("timestamp asc").Find(&rows).Error; err != nil {
		t.Fatalf("query memory_ops: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (append-only, not upserted)", len(rows))
	}
	if rows[0].Op != "add" || rows[0].Actor != "consolidator" {
		t.Fatalf("row 0 = %+v, want op=add actor=consolidator", rows[0])
	}
	if rows[1].Op != "invalidate" || rows[1].Actor != "outcome-feedback" || rows[1].Reason != "pr closed unmerged" {
		t.Fatalf("row 1 = %+v, want op=invalidate actor=outcome-feedback reason=%q", rows[1], "pr closed unmerged")
	}
	if rows[0].ID == "" || rows[1].ID == "" || rows[0].ID == rows[1].ID {
		t.Fatalf("rows must each get a distinct generated id: %q vs %q", rows[0].ID, rows[1].ID)
	}
}

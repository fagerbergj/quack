package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"

	"github.com/fagerbergj/quack/internal/dag"
)

// TestResumePausedDagNodes_SkipsChatRowGone pins #1296: a paused node whose
// owning chat row is gone (raw SQL bypassing DeleteChat's cascade, or legacy
// data from before the chats(id) FK existed) must never be resumed - resuming
// it would write chat_events for a chat with no row, exactly the prod orphan.
func TestResumePausedDagNodes_SkipsChatRowGone(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	st, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := st.db.Create(&Chat{ID: "c1"}).Error; err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusPaused), PauseReason: string(dag.PauseUser)}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	// Reproduce the exact orphan shape prod hit: chat row gone, dag_plans/
	// dag_nodes still there. With the chats(id) FK live this can no longer
	// happen through this store's own connection - disable enforcement to
	// simulate a pre-#1296 database or a raw SQL delete run with
	// constraints off.
	if err := st.db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		t.Fatalf("disable FK: %v", err)
	}
	if err := st.db.Exec("DELETE FROM chats WHERE id = ?", "c1").Error; err != nil {
		t.Fatalf("delete chat row: %v", err)
	}

	rep, err := st.ResumePausedDagNodes(ctx, nil)
	if err != nil {
		t.Fatalf("ResumePausedDagNodes: %v", err)
	}
	if len(rep.Start) != 0 || len(rep.AwaitingInput) != 0 {
		t.Fatalf("resume report = %+v, want nothing started/awaiting for a chat with no row", rep)
	}
	if len(rep.Failed) != 1 || rep.Failed[0].Reason != "chat row is gone" {
		t.Fatalf("Failed = %+v, want one node failed with \"chat row is gone\"", rep.Failed)
	}

	exists, err := st.ChatEventsExist(ctx, "c1")
	if err != nil {
		t.Fatalf("ChatEventsExist: %v", err)
	}
	if exists {
		t.Fatalf("chat_events written for a chat with no row")
	}
}

// TestDeleteChatRow_CascadesPerChatTables pins #1296's DB-level guarantee: a
// raw SQL DELETE against chats (bypassing DeleteChat's app-level cascade
// entirely) still removes every per-chat row, via ON DELETE CASCADE, not
// application code.
func TestDeleteChatRow_CascadesPerChatTables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	st, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	const chatID = "c1"
	if err := st.db.Create(&Chat{ID: chatID}).Error; err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if err := st.SaveTurn(ctx, chatID, "t1", ""); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := st.SaveDagPlan(ctx, chatID, "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.InsertChatEvent(ctx, ChatEvent{ChatID: chatID, Seq: 1, Event: "{}"}); err != nil {
		t.Fatalf("InsertChatEvent: %v", err)
	}
	if err := st.SetProjectionWatermark(ctx, chatID, "sse", 1); err != nil {
		t.Fatalf("SetProjectionWatermark: %v", err)
	}
	if err := st.upsertCheckpoint(ctx, chatID, 1, []byte(`{}`)); err != nil {
		t.Fatalf("upsertCheckpoint: %v", err)
	}

	// Raw SQL, not DeleteChat - the exact bypass guards against.
	if err := st.db.Exec("DELETE FROM chats WHERE id = ?", chatID).Error; err != nil {
		t.Fatalf("raw delete chats row: %v", err)
	}

	var turnCount, planCount, eventCount, watermarkCount, checkpointCount int64
	st.db.Model(&ChatTurn{}).Where("chat_id = ?", chatID).Count(&turnCount)
	st.db.Model(&DagPlan{}).Where("chat_id = ?", chatID).Count(&planCount)
	st.db.Model(&ChatEvent{}).Where("chat_id = ?", chatID).Count(&eventCount)
	st.db.Model(&ProjectionWatermark{}).Where("chat_id = ?", chatID).Count(&watermarkCount)
	st.db.Model(&Checkpoint{}).Where("chat_id = ?", chatID).Count(&checkpointCount)
	if turnCount != 0 || planCount != 0 || eventCount != 0 || watermarkCount != 0 || checkpointCount != 0 {
		t.Fatalf("dependents survived a raw chats delete: turns=%d plans=%d events=%d watermarks=%d checkpoints=%d",
			turnCount, planCount, eventCount, watermarkCount, checkpointCount)
	}
}

// TestNew_SweepsPreExistingOrphansBeforeMigrating pins the boot-safety half
// of #1296: prod already had 1,367 orphan chat_events rows (chats hard-deleted
// before this FK existed) when this migration ships. Without a sweep,
// AutoMigrate's ALTER TABLE ADD CONSTRAINT (Postgres) / table-rebuild
// (SQLite) fails validating a pre-existing orphan, and the failure repeats
// every boot since the orphan survives a restart.
func TestNew_SweepsPreExistingOrphansBeforeMigrating(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	raw, err := sql.Open(sqlite.DriverName, dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	// Minimal pre-#1296 schema: no FK yet, just enough for AutoMigrate to see
	// the table exists and try to add the constraint against it.
	if _, err := raw.Exec(`CREATE TABLE chats (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create chats: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE chat_events (chat_id TEXT, seq INTEGER, event TEXT, created_at DATETIME, PRIMARY KEY (chat_id, seq))`); err != nil {
		t.Fatalf("create chat_events: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO chats (id) VALUES ('live1')`); err != nil {
		t.Fatalf("insert live chat: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO chat_events (chat_id, seq, event, created_at) VALUES ('orphan1', 1, '{}', datetime('now'))`); err != nil {
		t.Fatalf("insert orphan event: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	st, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New: %v (a pre-existing orphan must not fail boot)", err)
	}

	var orphanCount, liveCount int64
	st.db.Model(&ChatEvent{}).Where("chat_id = ?", "orphan1").Count(&orphanCount)
	st.db.Model(&Chat{}).Where("id = ?", "live1").Count(&liveCount)
	if orphanCount != 0 {
		t.Errorf("orphan chat_events row survived migration: count=%d", orphanCount)
	}
	if liveCount != 1 {
		t.Errorf("unrelated chat row was dropped by the sweep: count=%d", liveCount)
	}
}

package store

import (
	"context"
	"path/filepath"
	"testing"

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

	// Raw SQL, not DeleteChat - the exact bypass #1296 guards against.
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

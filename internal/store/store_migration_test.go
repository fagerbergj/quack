package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/pgtest"
)

// New()'s CREATE INDEX IF NOT EXISTS must not error on a second boot against a migrated database.
func TestNew_EventsSessionIndex_IdempotentAcrossBoots(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")

	st1, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (boot 1): %v", err)
	}
	st2, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (boot 2): %v", err)
	}

	var count int64
	if err := st2.db.Raw("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_events_session_lookup'").Scan(&count).Error; err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 1 {
		t.Errorf("idx_events_session_lookup count = %d, want 1 (idempotent, not duplicated)", count)
	}
	_ = st1
}

// Open and NewArtifactService(url, false) serve read-only CLIs, so neither may create a table.
func TestOpen_RunsNoMigrations(t *testing.T) {
	st, err := Open("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var tables int64
	if err := st.db.Raw("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("Open created %d table(s), want 0", tables)
	}

	svc, err := NewArtifactService(pgtest.DSN(t), false)
	if err != nil {
		t.Fatalf("NewArtifactService: %v", err)
	}
	if svc.(*gormArtifactService).db.Migrator().HasTable(&Artifact{}) {
		t.Error("NewArtifactService(url, false) created the artifacts table")
	}
}

// The two-boot check against real Postgres, whose index/AutoMigrate failure modes differ from sqlite.
// Skips without Docker.
func TestNew_Migrations_IdempotentAcrossBoots_Postgres(t *testing.T) {
	dsn := pgtest.DSN(t)
	if _, err := New("postgres", dsn); err != nil {
		t.Fatalf("New (boot 1): %v", err)
	}
	st2, err := New("postgres", dsn)
	if err != nil {
		t.Fatalf("New (boot 2): %v", err)
	}

	var idxCount int64
	if err := st2.db.Raw("SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_events_session_lookup'").Scan(&idxCount).Error; err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("idx_events_session_lookup count = %d, want 1", idxCount)
	}
	if err := st2.db.Raw("SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_dag_nodes_plan_id'").Scan(&idxCount).Error; err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("idx_dag_nodes_plan_id count = %d, want 1", idxCount)
	}
}

// The cascade check on real Postgres, where FKs come from ALTER TABLE ADD CONSTRAINT rather than sqlite's
// table rebuild. Skips without Docker.
func TestDeleteChatRow_CascadesPerChatTables_Postgres(t *testing.T) {
	ctx := context.Background()
	st, err := New("postgres", pgtest.DSN(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

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

	// Raw SQL, not DeleteChat: the exact bypass the FK guards against.
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

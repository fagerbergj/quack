package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestNew_EventsSessionIndex_IdempotentAcrossBoots pins perf audit #3's fix:
// the CREATE INDEX IF NOT EXISTS in New() must not error on a second boot
// against the same (already-migrated) database.
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

// TestNew_Migrations_IdempotentAcrossBoots_Postgres runs the same two-boot
// check against real Postgres, where CREATE INDEX IF NOT EXISTS and GORM's
// AutoMigrate (dag_nodes.plan_id index) have different failure modes than
// sqlite. Skips if Docker isn't reachable.
func TestNew_Migrations_IdempotentAcrossBoots_Postgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_migration_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping postgres migration test: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

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

// TestDeleteChatRow_CascadesPerChatTables_Postgres is
// TestDeleteChatRow_CascadesPerChatTables against real Postgres :
// AutoMigrate's relationship-driven FK creation (ALTER TABLE ADD CONSTRAINT)
// is a different code path than glebarez/sqlite's table-rebuild one, so the
// cascade needs its own proof on the dialect prod actually runs. Skips if
// Docker isn't reachable.
func TestDeleteChatRow_CascadesPerChatTables_Postgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_cascade_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping postgres cascade test: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	st, err := New("postgres", dsn)
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

package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/gorm"
)

// TestSweepOrphanChatRows_SkipsOnceConstraintExists pins perf audit #11: once the
// chats(id) ON DELETE CASCADE FK exists (every boot after the first), sweepOrphanChatRows
// must issue no DELETE at all, instead of re-scanning every FK'd table for nothing.
// Counting via a Raw callback (db.Exec goes through gorm's Raw processor, unlike Query)
// observes the SQL that actually ran, rather than inferring it from row counts - deleting
// 0 rows looks identical to never running.
func TestSweepOrphanChatRows_SkipsOnceConstraintExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quack.db")
	if _, err := New("sqlite", path); err != nil {
		t.Fatalf("New: %v", err)
	}

	dialector, err := dialectorFor("sqlite", path)
	if err != nil {
		t.Fatalf("dialectorFor: %v", err)
	}
	db, err := gorm.Open(dialector(), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}

	var deletes int
	if err := db.Callback().Raw().After("gorm:raw").Register("test:count_deletes", func(tx *gorm.DB) {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(tx.Statement.SQL.String())), "DELETE") {
			deletes++
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	if err := sweepOrphanChatRows(db); err != nil {
		t.Fatalf("sweepOrphanChatRows: %v", err)
	}
	if deletes != 0 {
		t.Errorf("sweepOrphanChatRows issued %d DELETE(s) on a DB whose FK constraint already exists, want 0", deletes)
	}
}

// TestSweepOrphanChatRows_RunsOnFreshDB is the negative case: a DB with the FK tables but
// no constraint yet (pre-migration) must still get swept, or a pre-existing orphan survives
// to break AutoMigrate's later ALTER TABLE ADD CONSTRAINT.
func TestSweepOrphanChatRows_RunsOnFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quack.db")
	dialector, err := dialectorFor("sqlite", path)
	if err != nil {
		t.Fatalf("dialectorFor: %v", err)
	}
	db, err := gorm.Open(dialector(), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	// Migrate the tables without their CASCADE FK (Chat is a plain struct here, not the
	// constraint-carrying field) - mirrors the pre-#1296 schema sweepOrphanChatRows exists for.
	if err := db.Exec(`CREATE TABLE chats (id TEXT PRIMARY KEY)`).Error; err != nil {
		t.Fatalf("create chats: %v", err)
	}
	if err := db.Exec(`CREATE TABLE chat_events (chat_id TEXT, seq INTEGER, event TEXT, created_at DATETIME)`).Error; err != nil {
		t.Fatalf("create chat_events: %v", err)
	}
	if err := db.Exec(`INSERT INTO chats (id) VALUES ('c1')`).Error; err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if err := db.Exec(`INSERT INTO chat_events (chat_id, seq, event, created_at) VALUES ('orphan', 1, '{}', datetime('now'))`).Error; err != nil {
		t.Fatalf("insert orphan: %v", err)
	}

	if err := sweepOrphanChatRows(db); err != nil {
		t.Fatalf("sweepOrphanChatRows: %v", err)
	}

	var n int64
	if err := db.Table("chat_events").Where("chat_id = ?", "orphan").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("orphan row survived sweepOrphanChatRows on a pre-constraint DB, want swept")
	}
}

// TestSweepOrphanChatRows_Postgres proves HasConstraint's "Chat" field lookup - which
// resolves to a driver-agnostic generated FK name via GORM's own relationship reflection,
// not a literal constraint-name string - actually round-trips on Postgres, not only sqlite.
// A false negative here would only cost the 400-510ms scan; a false positive (constraint
// reported present when it isn't) means the sweep never runs on a first Postgres boot with
// real orphans, which is a boot crash loop, not a slow boot (#1307's reviewer flagged this
// exact risk). Covers both orders: pre-migration (sweep must still run) and post-AutoMigrate
// (sweep must skip). Skips if Docker isn't reachable.
func TestSweepOrphanChatRows_Postgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_sweep_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping postgres sweep test: %v", err)
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

	dialector, err := dialectorFor("postgres", dsn)
	if err != nil {
		t.Fatalf("dialectorFor: %v", err)
	}

	t.Run("pre-migration DB still gets swept", func(t *testing.T) {
		db, err := gorm.Open(dialector(), &gorm.Config{})
		if err != nil {
			t.Fatalf("gorm.Open: %v", err)
		}
		// Same shape as the sqlite negative case: tables exist, no FK yet.
		if err := db.Exec(`CREATE TABLE chats (id TEXT PRIMARY KEY)`).Error; err != nil {
			t.Fatalf("create chats: %v", err)
		}
		if err := db.Exec(`CREATE TABLE chat_events (chat_id TEXT, seq BIGINT, event TEXT, created_at TIMESTAMPTZ)`).Error; err != nil {
			t.Fatalf("create chat_events: %v", err)
		}
		if err := db.Exec(`INSERT INTO chats (id) VALUES ('c1')`).Error; err != nil {
			t.Fatalf("insert chat: %v", err)
		}
		if err := db.Exec(`INSERT INTO chat_events (chat_id, seq, event, created_at) VALUES ('orphan', 1, '{}', now())`).Error; err != nil {
			t.Fatalf("insert orphan: %v", err)
		}

		if err := sweepOrphanChatRows(db); err != nil {
			t.Fatalf("sweepOrphanChatRows: %v", err)
		}

		var n int64
		if err := db.Table("chat_events").Where("chat_id = ?", "orphan").Count(&n).Error; err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("orphan row survived sweepOrphanChatRows on a pre-constraint Postgres DB, want swept")
		}

		// Clean slate for the next subtest: AutoMigrate below must create its own
		// chats/chat_events, not inherit these FK-less ones.
		if err := db.Exec(`DROP TABLE chat_events, chats`).Error; err != nil {
			t.Fatalf("drop tables: %v", err)
		}
	})

	t.Run("migrated DB skips the sweep", func(t *testing.T) {
		if _, err := New("postgres", dsn); err != nil {
			t.Fatalf("New: %v", err)
		}

		db, err := gorm.Open(dialector(), &gorm.Config{})
		if err != nil {
			t.Fatalf("gorm.Open: %v", err)
		}
		var deletes int
		if err := db.Callback().Raw().After("gorm:raw").Register("test:count_deletes", func(tx *gorm.DB) {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(tx.Statement.SQL.String())), "DELETE") {
				deletes++
			}
		}); err != nil {
			t.Fatalf("register callback: %v", err)
		}

		if err := sweepOrphanChatRows(db); err != nil {
			t.Fatalf("sweepOrphanChatRows: %v", err)
		}
		if deletes != 0 {
			t.Errorf("sweepOrphanChatRows issued %d DELETE(s) on a migrated Postgres DB, want 0", deletes)
		}
	})
}

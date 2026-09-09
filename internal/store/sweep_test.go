package store

import (
	"path/filepath"
	"strings"
	"testing"

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

package ledger

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// ledgerSeedPath is the perf audit's seeded ledger (93 chats x 5,000 entries, ~8.6KB
// llm.call payloads, 4.0GB) - read-only scratch data, not built by this repo. Benchmarks
// below skip when it's absent, which is the normal case off the audit's own machine.
const ledgerSeedPath = "/home/jason/workspace/wt/audit/scratch-perf/ledger.db"

func openLedgerSeedDB(tb testing.TB) *gorm.DB {
	tb.Helper()
	if _, err := os.Stat(ledgerSeedPath); err != nil {
		tb.Skip("ledger seed db missing; perf-audit scratch DB not present on this machine")
	}
	sqlDB, err := sql.Open("sqlite", ledgerSeedPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		tb.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	db, err := gorm.Open(&sqlite.Dialector{Conn: sqlDB}, &gorm.Config{})
	if err != nil {
		tb.Fatal(err)
	}
	return db
}

// BenchmarkMemoryStatsOldPerChatLoop is the pre-fix shape memoryLedgerEvents used: a
// GROUP BY chat_id scan (List()'s own query, inlined here with a string Last column - the
// driver pair this benchmark runs under can't Scan MAX(at) straight into time.Time) plus
// one ReadEntriesFiltered per chat (perf audit #12).
func BenchmarkMemoryStatsOldPerChatLoop(b *testing.B) {
	db := openLedgerSeedDB(b)
	s := &PGStore{db: db}
	ctx := context.Background()
	kinds := []string{KindMemoryVote, KindMemoryRecall}
	b.ReportAllocs()
	for b.Loop() {
		var aggs []struct {
			ChatID string
			Cnt    int64
			Last   string
		}
		if err := db.WithContext(ctx).Model(&pgEntry{}).
			Select("chat_id, count(*) as cnt, max(at) as last").Group("chat_id").Scan(&aggs).Error; err != nil {
			b.Fatal(err)
		}
		total := 0
		for _, a := range aggs {
			es, err := s.ReadEntriesFiltered(ctx, a.ChatID, 0, kinds)
			if err != nil {
				b.Fatal(err)
			}
			total += len(es)
		}
		if total == 0 {
			b.Fatal("no entries")
		}
	}
}

// BenchmarkMemoryStatsOneQuery is the fix: one cross-chat `kind IN (?) AND at >= ?` query,
// with the 12-week window pushed into SQL instead of filtered in Go (perf audit #12).
func BenchmarkMemoryStatsOneQuery(b *testing.B) {
	db := openLedgerSeedDB(b)
	s := &PGStore{db: db}
	ctx := context.Background()
	kinds := []string{KindMemoryVote, KindMemoryRecall}
	since := time.Now().UTC().AddDate(0, 0, -7*12)
	b.ReportAllocs()
	for b.Loop() {
		es, err := s.ReadEntriesFilteredSince(ctx, kinds, since)
		if err != nil {
			b.Fatal(err)
		}
		if len(es) == 0 {
			b.Fatal("no entries")
		}
	}
}

package store

import (
	"context"
	"os"
	"testing"
)

// prodSeedPath is a seeded prod-shaped sqlite DB (93 chats, 14,050 session events on chat-0000), not built
// by this repo; the benchmarks skip when it's absent.
const prodSeedPath = "/home/jason/workspace/wt/audit/scratch-perf/prod.db"

func openProdSeedStore(tb testing.TB) *Store {
	tb.Helper()
	if _, err := os.Stat(prodSeedPath); err != nil {
		tb.Skip("prod seed db missing; perf-audit scratch DB not present on this machine")
	}
	s, err := New("sqlite", prodSeedPath)
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// The run-end path must stay cheap on a large session, unlike GetTurnsWithContent's whole-session decode.
func BenchmarkGetLastTurnWithContent(b *testing.B) {
	s := openProdSeedStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		tc, err := s.GetLastTurnWithContent(ctx, "quack", "user", "chat-0000")
		if err != nil {
			b.Fatal(err)
		}
		if tc == nil {
			b.Fatal("nil turn")
		}
	}
}

// BenchmarkGetTurnsWithContent is the whole-chat load, for comparison with BenchmarkGetLastTurnWithContent.
func BenchmarkGetTurnsWithContent(b *testing.B) {
	s := openProdSeedStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		tc, err := s.GetTurnsWithContent(ctx, "quack", "user", "chat-0000")
		if err != nil {
			b.Fatal(err)
		}
		if len(tc) != 50 {
			b.Fatalf("turns=%d", len(tc))
		}
	}
}

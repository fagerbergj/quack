package store

import (
	"context"
	"os"
	"testing"
)

// prodSeedPath is the perf audit's seeded prod-shaped sqlite DB (93 chats,
// ~1.35M chat_events, 14,050 ADK session events on chat-0000) - read-only
// scratch data, not built by this repo; benchmarks below skip when it's absent, the normal case off the audit's own machine.
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

// BenchmarkGetLastTurnWithContent pins perf audit #3's fix: the run-end path
// (StampTerminalOutcome, stampRunOutcome) must stay cheap on a chat whose
// session has grown large, unlike GetTurnsWithContent (BenchmarkGetTurnsWithContent below) which decodes and allocates for the whole 14,050-event session every time.
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

// BenchmarkGetTurnsWithContent is the pre-fix shape the run-end paths used to pay for on
// every call: the whole chat's ADK session loaded and decoded, all 50 turns' worth, just to
// read the last one. Compare against BenchmarkGetLastTurnWithContent above.
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

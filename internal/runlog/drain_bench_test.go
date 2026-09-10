package runlog

import (
	"context"
	"fmt"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// newTestPostgresStore starts a real Postgres container (perf-audit's own methodology:
// single-row inserts are RTT-bound, so only a real network round trip - not sqlite -
// reproduces the ceiling being fixed here). Skips when Docker isn't reachable.
func newTestPostgresStore(b *testing.B) *store.Store {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_runlog_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		b.Skipf("docker unavailable, skipping postgres benchmark: %v", err)
	}
	b.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			b.Logf("terminate postgres container: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		b.Fatalf("connection string: %v", err)
	}
	st, err := store.New("postgres", dsn)
	if err != nil {
		b.Fatalf("store.New: %v", err)
	}
	return st
}

// BenchmarkDrainThroughput is the audit's own measurement (perf-audit item 2): single-row
// INSERT-per-event vs the batched drain, against a real Postgres container. A
// Benchmark, not a Test, on purpose - throughput is inherently a wall-clock,
// hardware-dependent number, so it must never run under plain `go test` (including `-count=200`); `go test -bench` opts in. The correctness properties (nothing dropped, nothing duplicated) live in the deterministic tests below instead.
func BenchmarkDrainThroughput(b *testing.B) {
	st := newTestPostgresStore(b)
	ctx := context.Background()
	const n = 3000

	// Before: one InsertChatEvent round trip per event (the pre-fix path).
	before := time.Now()
	for i := int64(1); i <= n; i++ {
		ev := store.ChatEvent{ChatID: "bench-single", Seq: i, Event: `{"name":"token","data":{"text":"x"}}`, CreatedAt: time.Now().UTC()}
		if err := st.InsertChatEvent(ctx, ev); err != nil {
			b.Fatalf("InsertChatEvent: %v", err)
		}
	}
	beforeElapsed := time.Since(before)

	// After: batched into drainBatchSize-row INSERTs (the fixed path).
	after := time.Now()
	var batch []store.ChatEvent
	for i := int64(1); i <= n; i++ {
		batch = append(batch, store.ChatEvent{ChatID: "bench-batch", Seq: i, Event: `{"name":"token","data":{"text":"x"}}`, CreatedAt: time.Now().UTC()})
		if len(batch) == drainBatchSize {
			if err := st.InsertChatEvents(ctx, batch); err != nil {
				b.Fatalf("InsertChatEvents: %v", err)
			}
			batch = batch[:0]
		}
	}
	if err := st.InsertChatEvents(ctx, batch); err != nil {
		b.Fatalf("InsertChatEvents (final): %v", err)
	}
	afterElapsed := time.Since(after)

	beforeRate := float64(n) / beforeElapsed.Seconds()
	afterRate := float64(n) / afterElapsed.Seconds()
	fmt.Printf("perf-audit item 2: before (single-row) %.0f ev/s (%v for %d), after (batched, size=%d) %.0f ev/s (%v for %d)\n",
		beforeRate, beforeElapsed, n, drainBatchSize, afterRate, afterElapsed, n)
}

// TestExactlyOnceResumeAcrossBatchBoundary proves the durable log has every event
// exactly once even when a run's events straddle a drain batch boundary - the
// correctness property batching must not break. sqlite, not postgres: this must run fast and deterministically under -race -count=200 without Docker (the throughput comparison above is the only thing that needs a real network round trip).
func TestExactlyOnceResumeAcrossBatchBoundary(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	l := NewEventLog(st)
	chatID := "chat-boundary"
	mustSeedChat(t, st, chatID)

	// More than one batch's worth, so the drain goroutine must flush at
	// least twice, with a boundary landing mid-run.
	const total = drainBatchSize*2 + 37
	for i := int64(1); i <= total; i++ {
		l.Append(chatID, i, stream.SSEEvent{Name: "token", Data: map[string]any{"i": i}})
	}
	l.Flush()

	evs, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents: %v", err)
	}
	if len(evs) != total {
		t.Fatalf("got %d events, want %d (exactly once)", len(evs), total)
	}
	for i, e := range evs {
		wantSeq := int64(i + 1)
		if e.Seq != wantSeq {
			t.Fatalf("event[%d].Seq = %d, want %d (gap or dup at a batch boundary)", i, e.Seq, wantSeq)
		}
	}

	// A reconnecting subscriber with Last-Event-ID mid-batch must see every
	// later event exactly once, none skipped or repeated.
	mid := int64(drainBatchSize + 5)
	resumed, err := st.LoadChatEvents(ctx, chatID, mid)
	if err != nil {
		t.Fatalf("LoadChatEvents (resume): %v", err)
	}
	if int64(len(resumed)) != total-mid {
		t.Fatalf("resume from seq %d: got %d events, want %d", mid, len(resumed), total-mid)
	}
	if resumed[0].Seq != mid+1 {
		t.Fatalf("resume from seq %d: first event seq = %d, want %d", mid, resumed[0].Seq, mid+1)
	}
}

// TestFinishRunDeliversBufferedTailBeforeUnregister is the review's correctness
// follow-up: a run finishing with events still sitting in the batch queue (not yet
// drained to the DB) must have every one of them durably persisted, AND delivered to an already-live subscriber, by the time the run is unregistered - not merely by the time FinishRun returns. Deterministic by construction, not by timing: FinishRun runs Flush (which blocks on the actual DB write) strictly before Close/cancelRun/Unregister, on a single goroutine. So a concurrent observer that spins on HasRegisteredRun and reads the store the instant it flips false is guaranteed - via plain program order on FinishRun's goroutine, not luck - to see every event already committed. Reordering FinishRun's steps (the bug this test guards against: three of five call sites unregistered before flushing) would make this test observably flaky/failing, exactly the shutdown-drain race (serve.DrainActiveRuns polls HasRegisteredRun to decide a run is safe to consider over).
func TestFinishRunDeliversBufferedTailBeforeUnregister(t *testing.T) {
	st := newTestStore(t)
	hub := stream.NewHub()
	l := NewEventLog(st)
	chatID := "chat-finish-tail"
	mustSeedChat(t, st, chatID)

	runCtx, cancelRun := context.WithCancel(context.Background())
	hub.RegisterRun(chatID, "turn-1", cancelRun)

	replay, live, unsubscribe, done := hub.Subscribe(chatID)
	if done || len(replay) != 0 {
		t.Fatalf("fresh subscribe: done=%v replay=%d, want live + empty", done, len(replay))
	}
	defer unsubscribe()

	const n = 500 // large enough that the batched DB write takes many spin-loop iterations
	pub := NewPublisher(hub, l, chatID)
	for i := 0; i < n; i++ {
		pub.Publish(stream.SSEEvent{Name: "token", Data: map[string]any{"i": i}})
	}

	// Emulates serve.DrainActiveRuns.waitWhileAnyRegistered: the moment the
	// run is unregistered, trust its tail is fully durable.
	seenAtUnregister := make(chan int, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for hub.HasRegisteredRun(chatID) {
			if time.Now().After(deadline) {
				seenAtUnregister <- -1
				return
			}
		}
		evs, _ := st.LoadChatEvents(context.Background(), chatID, 0)
		seenAtUnregister <- len(evs)
	}()

	l.FinishRun(hub, chatID, "turn-1", cancelRun)

	if runCtx.Err() == nil {
		t.Fatal("FinishRun must cancel the run context")
	}
	if got := <-seenAtUnregister; got != n {
		t.Fatalf("store had %d events the instant the run unregistered, want all %d durable first", got, n)
	}

	// Delivered to the subscriber that was live before the run ended, too.
	got := 0
	for range live {
		got++
	}
	if got != n {
		t.Fatalf("live subscriber saw %d events, want %d", got, n)
	}
}

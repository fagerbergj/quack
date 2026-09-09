package runlog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// TestBatchInsertFailureDoesNotDropGoodRows: a single bad row (e.g. a
// duplicate (chat_id, seq) from a Reset/retry race) must not take the other
// ~199 good rows in its batch down with it - InsertChatEvents is one SQL
// statement, so a PK conflict on any row fails the whole statement unless the
// drain falls back to per-row isolation.
func TestBatchInsertFailureDoesNotDropGoodRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	l := NewEventLog(st)
	chatID := "chat-bad-row"

	// Seed seq=5 directly so the batch below collides on the primary key.
	if err := st.InsertChatEvent(ctx, store.ChatEvent{ChatID: chatID, Seq: 5, Event: `{"name":"token","data":{}}`, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed InsertChatEvent: %v", err)
	}

	const n = 10
	for i := int64(1); i <= n; i++ {
		l.Append(chatID, i, stream.SSEEvent{Name: "token", Data: map[string]any{"i": i}})
	}
	l.Flush()

	evs, err := st.LoadChatEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents: %v", err)
	}
	got := map[int64]bool{}
	for _, e := range evs {
		got[e.Seq] = true
	}
	for i := int64(1); i <= n; i++ {
		if i == 5 {
			continue // the genuinely conflicting row is expected to be dropped
		}
		if !got[i] {
			t.Errorf("seq %d missing after a batch insert failure on seq 5 - a batch failure must not drop the whole batch", i)
		}
	}
}

// TestAppendRacingFinishRunNeverLostSilently: an event Append'd on the same
// chat concurrently with FinishRun's Flush (e.g. a still-unwinding tool
// goroutine writing after cancelRun fires) is not covered by that Flush's
// "everything enqueued before this call" contract, but the shared drain
// channel has no path that discards it either - a later drain of the
// channel (here, one more Flush) must still find it durably persisted.
func TestAppendRacingFinishRunNeverLostSilently(t *testing.T) {
	st := newTestStore(t)
	hub := stream.NewHub()
	l := NewEventLog(st)
	chatID := "chat-race-finish"

	runCtx, cancelRun := context.WithCancel(context.Background())
	hub.RegisterRun(chatID, "turn-1", cancelRun)

	pub := NewPublisher(hub, l, chatID)
	for i := int64(1); i <= 5; i++ {
		pub.Publish(stream.SSEEvent{Name: "token", Data: map[string]any{"i": i}})
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		l.Append(chatID, 999, stream.SSEEvent{Name: "token", Data: map[string]any{"late": true}})
	}()

	l.FinishRun(hub, chatID, cancelRun)
	wg.Wait()
	if runCtx.Err() == nil {
		t.Fatal("FinishRun must cancel the run context")
	}
	l.Flush() // drain goroutine never exits; one more flush proves the race landed in the queue, not the void

	evs, err := st.LoadChatEvents(context.Background(), chatID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents: %v", err)
	}
	for _, e := range evs {
		if e.Seq == 999 {
			return
		}
	}
	t.Fatal("event appended concurrently with FinishRun was silently lost - never persisted")
}

package serve

import (
	"context"
	"fmt"
	"iter"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/stream"
)

// settledGoroutines returns runtime.NumGoroutine() once it stops changing -
// scheduler/GC goroutines come and go, so a single raw reading is flaky.
func settledGoroutines(t *testing.T) int {
	t.Helper()
	runtime.GC()
	last := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == last {
			return n
		}
		last = n
	}
	return last
}

// TestDriveExtensionRunEvents_SharesEventLogAcrossRuns is the regression test
// for the leak fixed alongside #1292: runlog.NewEventLog per dispatched run
// each spun up a drain goroutine that FinishRun never stops, since EventLog has no Close. Dispatching many runs against one EventLog (as serve.Run's single bootEventLog does in production) must not multiply that goroutine.
func TestDriveExtensionRunEvents_SharesEventLogAcrossRuns(t *testing.T) {
	st := newShutdownTestStore(t)
	hub := stream.NewHub()
	eventLog := runlog.NewEventLog(st)

	obs := &fakeRunObserver{}
	var ext extsdk.Extension = obs
	var extHolder atomic.Pointer[extsdk.Extension]
	extHolder.Store(&ext)

	baseline := settledGoroutines(t)

	const n = 50
	for i := 0; i < n; i++ {
		chatID := fmt.Sprintf("ext:noop:leak-%d", i)
		if err := st.SetChatOrigin(context.Background(), chatID, "ext", ""); err != nil {
			t.Fatalf("SetChatOrigin: %v", err)
		}
		entered := make(chan struct{})
		run := func(runCtx context.Context) iter.Seq2[stream.SSEEvent, error] {
			return func(yield func(stream.SSEEvent, error) bool) {
				close(entered)
				<-runCtx.Done()
			}
		}
		done := make(chan struct{})
		go func() {
			driveExtensionRunEvents(context.Background(), "noop", nil, st, hub, eventLog, &extHolder, "ext", chatID, "turn-1", 0, run)
			close(done)
		}()
		<-entered
		hub.MarkInterrupted(chatID)
		hub.CancelRun(chatID)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: driveExtensionRunEvents did not return after cancel", i)
		}
	}

	after := settledGoroutines(t)
	if leaked := after - baseline; leaked > 3 {
		t.Errorf("goroutine count grew by %d after %d dispatched runs (baseline %d, after %d) - want ~1 shared drain goroutine, not one per run",
			leaked, n, baseline, after)
	}
}

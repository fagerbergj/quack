package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/stream"
)

// redirectSlogForTest points the default slog logger at buf and returns a
// func to restore the previous logger.
func redirectSlogForTest(buf *strings.Builder) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return func() { slog.SetDefault(prev) }
}

// A run cancelled while queued must never fall through to execution (#1016).
// Before the fix, acquireRun's acquired=false was ignored and the plan ran
// on a dead ctx - here that dereferences the zero-value Orchestrator's nil executor.
func TestRunNeverExecutesOnCancelledQueuedContext(t *testing.T) {
	o := &Orchestrator{}
	o.SetMaxActiveRuns(1)

	hold, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("failed to occupy the only slot")
	}
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []error, 1)
	go func() {
		var errs []error
		for _, err := range o.Run(ctx, "user", "chat-1", SourceApp, "hi", nil) {
			errs = append(errs, err)
		}
		done <- errs
	}()

	time.Sleep(50 * time.Millisecond) // let the goroutine queue behind the held slot
	cancel()

	select {
	case errs := <-done:
		if len(errs) == 0 {
			t.Fatal("expected an error event for a run cancelled while queued")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run never returned after its ctx was cancelled while queued")
	}
}

// The stopped latch (#1016): a panicking yield must be recovered once and
// never invoke yield again on the same call sequence.
func TestSafeYieldIsolatesPanicAndSurvives(t *testing.T) {
	var calls int32
	boom := errors.New("distinctive-original-panic-42")
	sy := newSafeYield(func(stream.SSEEvent, error) bool {
		atomic.AddInt32(&calls, 1)
		panic(boom)
	})

	// First call panics inside yield; must be recovered here, not propagate.
	if sy(stream.SSEEvent{}, nil) {
		t.Fatal("expected false from a panicking yield")
	}
	// A second caller (the racing goroutine in the real bug) must be stopped
	// cleanly, never re-invoking yield.
	if sy(stream.SSEEvent{}, nil) {
		t.Fatal("expected false after the stream already stopped on panic")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("yield invoked %d times, want exactly 1 - a second invocation is the masking bug", got)
	}
}

// The original panic value must reach the logs, not be silently swallowed.
func TestSafeYieldLogsOriginalPanicValue(t *testing.T) {
	var buf strings.Builder
	restore := redirectSlogForTest(&buf)
	defer restore()

	const marker = "distinctive-panic-value-for-log-assertion"
	sy := newSafeYield(func(stream.SSEEvent, error) bool { panic(marker) })
	sy(stream.SSEEvent{}, nil)

	if !strings.Contains(buf.String(), marker) {
		t.Fatalf("original panic value missing from logs: %q", buf.String())
	}
}

// A sequential two-call test cannot exercise the real bug (#1016): the second
// caller must be blocked on the mutex while the first panics, not called after.
func TestSafeYieldConcurrentPanicIsolatesAllCallers(t *testing.T) {
	const n = 8
	var buf strings.Builder
	restore := redirectSlogForTest(&buf)
	defer restore()

	var calls int32
	const boom = "distinctive-concurrent-panic-value"
	sy := newSafeYield(func(stream.SSEEvent, error) bool {
		atomic.AddInt32(&calls, 1) // mutex-serialized: only ever reached once, by whichever caller wins the race
		panic(boom)
	})

	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start // released together: forces real mutex contention, not a sequence
			results[i] = sy(stream.SSEEvent{}, nil)
		}(i)
	}
	ready.Wait() // every goroutine parked at the gate before any of them runs
	close(start)
	done.Wait()

	for i, ok := range results {
		if ok {
			t.Errorf("goroutine %d returned true - a panic must stop every caller, not just the one that hit it", i)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("yield body invoked %d times across %d concurrent callers, want exactly 1 - a second invocation into an already-panicked stream is the masking bug", got, n)
	}
	if !strings.Contains(buf.String(), boom) {
		t.Fatalf("original panic value missing from logs: %q", buf.String())
	}
}

package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

// A run queued behind a full MaxActiveRuns cap, then cancelled before a slot
// frees, must never fall through to plan execution (#1016). Before the fix,
// Run ignored acquireRun's acquired=false and ran the whole plan on a dead
// ctx - here that would dereference the zero-value Orchestrator's nil
// executor and panic instead of returning a clean error.
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

// The panic-masking bug (#1016): with >=2 goroutines yielding concurrently
// through the same mutex-guarded yield, a panicking loop body used to let a
// second, still-blocked goroutine call the same (already-panicked) yield
// again - Go's runtime then replaces the original panic with "range function
// continued iteration after loop body panic", destroying the diagnosis and
// killing the process. newSafeYield must recover the panic once, log the
// original value, and never invoke yield again on that stream.
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

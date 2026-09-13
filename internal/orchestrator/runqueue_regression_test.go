package orchestrator

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fagerbergj/quack/internal/stream"
)

// redirectSlogForTest points the default slog logger at buf and returns a
// func to restore the previous logger.
func redirectSlogForTest(buf *strings.Builder) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return func() { slog.SetDefault(prev) }
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

	// The panicking caller must see its own panic resumed: swallowing it here
	// makes the runtime panic at the range site instead, killing the process
	// (#1033). Survival belongs to startRun's goroutine, which owns the run.
	var got any
	func() {
		defer func() { got = recover() }()
		sy(stream.SSEEvent{}, nil)
	}()
	if got != boom { //nolint:errorlint // got is a recovered panic value (interface{}): errors.Is needs a typed error, identity is the point of the test
		t.Fatalf("panicking caller must observe the original panic, got %v", got)
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
	func() {
		defer func() { _ = recover() }() // resumed now (#1033), still logged first
		sy(stream.SSEEvent{}, nil)
	}()

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

	var calls, panicked int32
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
			// Exactly one caller reaches the panicking yield and has it resumed
			// (#1033); every other caller must be turned away with false.
			defer func() {
				if r := recover(); r != nil {
					atomic.AddInt32(&panicked, 1)
				}
			}()
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
	if got := atomic.LoadInt32(&panicked); got != 1 {
		t.Errorf("want exactly 1 caller to observe the resumed panic, got %d", got)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("yield body invoked %d times across %d concurrent callers, want exactly 1 - a second invocation into an already-panicked stream is the masking bug", got, n)
	}
	if !strings.Contains(buf.String(), boom) {
		t.Fatalf("original panic value missing from logs: %q", buf.String())
	}
}

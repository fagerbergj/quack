package rest

import (
	"log/slog"
	"strings"
	"testing"
)

// #1033: the run goroutines outlive the HTTP request, so chi's Recoverer never
// covers them - an unrecovered panic there takes the process, not just the run.
// recoverRun is the only thing standing between a panicking run and process
// death in the production shape, where the panic surfaces on a node goroutine
// rather than at the range site.
func TestRecoverRun_ContainsPanicAndLogsIt(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	const marker = "distinctive-run-panic-value"
	func() {
		defer recoverRun("chat-1", "turn-1") // must swallow, not re-panic
		panic(marker)
	}()

	log := buf.String()
	if !strings.Contains(log, marker) {
		t.Fatalf("panic value must be logged, got:\n%s", log)
	}
	for _, want := range []string{"chat-1", "turn-1"} {
		if !strings.Contains(log, want) {
			t.Errorf("log must identify the run (%q missing):\n%s", want, log)
		}
	}
}

// Cleanup defers registered BEFORE recoverRun must still run - the run has to
// be unregistered and its hub topic closed even when it panicked.
func TestRecoverRun_LeavesCleanupDefersRunning(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	var order []string
	func() {
		defer func() { order = append(order, "cleanup") }()
		defer recoverRun("chat-2", "turn-2")
		panic("boom")
	}()

	if len(order) != 1 || order[0] != "cleanup" {
		t.Fatalf("cleanup defer must still run after a recovered run panic, got %v", order)
	}
}

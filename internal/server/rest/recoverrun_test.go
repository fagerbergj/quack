package rest

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/store"
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

// The helper tests above pin recoverRun itself; this pins the WIRING. Deleting
// any `defer recoverRun(...)` line leaves those green but kills the process
// here, because a panicking run's cleanup defers never run and the hub topic
// stays open - subscribers hang forever instead of seeing the stream close.
func TestRunGoroutinePanic_StillClosesHubTopic(t *testing.T) {
	dp := &store.DagPlan{ID: "p1", TurnID: "t1"}
	for _, tc := range []struct {
		name  string
		start func(*Handler, string)
	}{
		{"startRun", func(h *Handler, c string) { h.startRun(c, "t1", "hi", nil) }},
		{"startNodeAsync", func(h *Handler, c string) { h.startNodeAsync(dp, c, "n1", "") }},
		{"retryNodeAsync", func(h *Handler, c string) { h.retryNodeAsync(dp, c, "n1", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))
			defer slog.SetDefault(prev)

			h := newTestHandler(t)
			chatID := mustCreateChat(t, h)
			_, live, cancel, _ := h.hub.Subscribe(chatID)
			defer cancel()

			h.orch = nil // ranging a nil orchestrator panics inside the run goroutine
			tc.start(h, chatID)

			for {
				select {
				case _, ok := <-live:
					if !ok {
						return // topic closed: the cleanup defers ran after the recover
					}
				case <-time.After(10 * time.Second):
					t.Fatal("hub topic never closed after a panicking run - subscribers hang")
				}
			}
		})
	}
}

package rest

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/store"
)

// Run goroutines outlive the request, so chi's Recoverer doesn't cover them: recoverRun is all that
// keeps a panic on a node goroutine from killing the process.
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

// This pins the wiring: dropping any `defer recoverRun(...)` kills the process here, since the run's cleanup
// defers never run and subscribers hang on an open hub topic.
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

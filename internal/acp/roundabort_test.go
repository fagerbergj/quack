package acp

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestRound_CancelNodeAbortsMidRound (#1030): a cancel arriving through
// RegisterRoundAbort - CancelNode's own line into the round, independent of
// ctx - must reach the subprocess via the same graceful-cancel/abort RPC as
// ctx.Done() already does, instead of waiting for the round to finish on
// its own.
func TestRound_CancelNodeAbortsMidRound(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var abort context.CancelFunc
	registered := make(chan struct{})
	unregistered := make(chan struct{})

	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=hang"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		RegisterRoundAbort: func(chatID, nodeID string, cancel context.CancelFunc) {
			if chatID != "chat1" || nodeID != "n1" {
				t.Errorf("RegisterRoundAbort got (%q,%q), want (chat1,n1)", chatID, nodeID)
			}
			mu.Lock()
			abort = cancel
			mu.Unlock()
			close(registered)
		},
		UnregisterRoundAbort: func(chatID, nodeID string) { close(unregistered) },
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	t0 := time.Now()
	go func() {
		// Parent ctx is background - never cancelled by the caller. If the
		// round only fires on ctx.Done(), this test hangs until IdleTimeout.
		done <- a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "loop forever", "chat1", "n1", "", "", func(eventSpec) bool { return true })
	}()

	<-registered
	mu.Lock()
	cancel := abort
	mu.Unlock()
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("want context cancellation from the round-abort cancel, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CancelNode's abort never reached the running round - it kept running")
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("abort took %v - not a mid-round interrupt", d)
	}
	select {
	case <-unregistered:
	case <-time.After(2 * time.Second):
		t.Fatal("UnregisterRoundAbort never called")
	}
}

// TestRound_CancelBeforePromptSpawn (#1030 review): a cancel arriving during
// the spawn/handshake window - before the prompt goroutine ever writes
// session/prompt - must bail immediately instead of sending session/cancel
// for a prompt that was never sent and then blocking for cancelGrace.
func TestRound_CancelBeforePromptSpawn(t *testing.T) {
	oldGrace := cancelGrace
	cancelGrace = 3 * time.Second
	defer func() { cancelGrace = oldGrace }()

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=hang"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		RegisterRoundAbort: func(chatID, nodeID string, cancel context.CancelFunc) {
			// Cancel synchronously, before round() reaches the prompt spawn -
			// reproducing the spawn/handshake-window race.
			cancel()
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	err = a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "loop forever", "chat1", "n1", "", "", func(eventSpec) bool { return true })
	if err == nil {
		t.Fatal("want an error from the pre-spawn cancel")
	}
	if d := time.Since(t0); d >= cancelGrace {
		t.Fatalf("round took %v (>= cancelGrace %v) - it waited on gracefulCancel for a prompt that was never sent", d, cancelGrace)
	}
}

// TestRound_CancelNodeAbortIdempotent (#1030): a second cancel() call, and a
// cancel() call after the round already returned, must not panic or block -
// context.CancelFunc is inherently idempotent, but the registration/cleanup
// wiring around it must not assume single-call.
func TestRound_CancelNodeAbortIdempotent(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var abort context.CancelFunc
	registered := make(chan struct{})

	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=hang"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		RegisterRoundAbort: func(chatID, nodeID string, cancel context.CancelFunc) {
			mu.Lock()
			abort = cancel
			mu.Unlock()
			close(registered)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "loop forever", "chat1", "n1", "", "", func(eventSpec) bool { return true })
	}()

	<-registered
	mu.Lock()
	cancel := abort
	mu.Unlock()

	// Double-cancel before the round has acknowledged: must not panic/block.
	cancel()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from the cancelled round")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("round never returned after a double cancel")
	}

	// Cancel arriving after the round already ended: still just a no-op.
	cancel()
}

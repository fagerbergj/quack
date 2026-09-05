package pgdial

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// setRetryBackoff is a test helper that sets RetryBackoff and restores it via
// t.Cleanup, so the package global is restored even on test failure/panic
// (#1200 review: manual end-of-test resets skip that on a t.Fatal).
func setRetryBackoff(t *testing.T, d time.Duration) {
	t.Helper()
	orig := RetryBackoff
	RetryBackoff = d
	t.Cleanup(func() { RetryBackoff = orig })
}

// TestWithDialRetry_RecoversAfterTransientFailures proves a dial-layer retry
// absorbs the #1193 DNS-blip pattern (fails a few seconds, then resolves).
func TestWithDialRetry_RecoversAfterTransientFailures(t *testing.T) {
	setRetryBackoff(t, time.Millisecond) // keep the test fast
	calls := 0
	dial := withDialRetry(func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("lookup quack-postgres: no such host")
		}
		return &net.TCPConn{}, nil
	})
	conn, err := dial(context.Background(), "tcp", "quack-postgres:5432")
	if err != nil {
		t.Fatalf("expected recovery on 3rd attempt, got err: %v", err)
	}
	if conn == nil {
		t.Fatal("expected a non-nil conn on success")
	}
	if calls != 3 {
		t.Fatalf("expected 3 dial attempts, got %d", calls)
	}
}

// TestWithDialRetry_GivesUpAfterMaxAttempts proves this only retries the dial
// itself (bounded), never turns into an unbounded loop - a query-level error
// never reaches this func at all, since it wraps net dial, not db/sql query execution.
func TestWithDialRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	setRetryBackoff(t, time.Millisecond)
	calls := 0
	wantErr := errors.New("lookup quack-postgres: no such host")
	dial := withDialRetry(func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls++
		return nil, wantErr
	})
	_, err := dial(context.Background(), "tcp", "quack-postgres:5432")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped dial error, got %v", err)
	}
	if calls != RetryAttempts {
		t.Fatalf("expected exactly %d attempts, got %d", RetryAttempts, calls)
	}
}

// TestWithDialRetry_ContextCanceledDuringBackoff proves shutdown is not held
// up by the backoff: canceling ctx while a retry is sleeping between attempts
// must return promptly with ctx.Err(), not run out the clock on all attempts
// or hang (#1200 review: the only branch that matters for a live server had
// no coverage - both prior tests used context.Background()).
func TestWithDialRetry_ContextCanceledDuringBackoff(t *testing.T) {
	setRetryBackoff(t, time.Hour) // would hang/timeout the test if cancellation didn't cut it short
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	dial := withDialRetry(func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls++
		cancel() // cancel on first failure, while the wrapper is about to sleep for backoff
		return nil, errors.New("connection refused")
	})

	done := make(chan struct{})
	var gotErr error
	go func() {
		_, gotErr = dial(ctx, "tcp", "quack-postgres:5432")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dial did not return promptly after context cancellation during backoff")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", gotErr)
	}
	if calls != 1 {
		t.Fatalf("expected the retry loop to stop after 1 attempt once canceled, got %d calls", calls)
	}
}

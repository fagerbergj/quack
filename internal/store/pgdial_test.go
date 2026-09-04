package store

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// TestWithDialRetry_RecoversAfterTransientFailures proves a dial-layer retry
// absorbs the #1193 DNS-blip pattern (fails a few seconds, then resolves).
func TestWithDialRetry_RecoversAfterTransientFailures(t *testing.T) {
	dialRetryBackoff = time.Millisecond // keep the test fast
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
	dialRetryBackoff = time.Millisecond
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
	if calls != dialRetryAttempts {
		t.Fatalf("expected exactly %d attempts, got %d", dialRetryAttempts, calls)
	}
}

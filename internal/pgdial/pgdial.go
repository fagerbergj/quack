// Package pgdial is a retrying postgres dial + GORM dialector, a leaf package so store and ledger can share
// it without an import cycle.
package pgdial

import (
	"context"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// RetryAttempts/RetryBackoff absorb a short DNS/dial blip (Docker's embedded DNS drops out for seconds).
// Only the TCP dial retries, never a query.
const RetryAttempts = 3

var RetryBackoff = 2 * time.Second

// withDialRetry gives a transient dial failure (DNS, connection refused) a few short retries; dial is
// injectable for tests.
func withDialRetry(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var lastErr error
		for attempt := 1; attempt <= RetryAttempts; attempt++ {
			conn, err := dial(ctx, network, addr)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if attempt == RetryAttempts {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(RetryBackoff):
			}
		}
		return nil, lastErr
	}
}

func retryingDialFunc() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{}
	return withDialRetry(d.DialContext)
}

// Open is the single construction point for every postgres GORM dialector, so dial retries apply once.
func Open(url string) (gorm.Dialector, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.DialFunc = retryingDialFunc()
	sqlDB := stdlib.OpenDB(*cfg)
	return postgres.New(postgres.Config{Conn: sqlDB}), nil
}

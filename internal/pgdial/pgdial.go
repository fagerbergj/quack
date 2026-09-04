// Package pgdial provides a retrying postgres dial + GORM dialector. It is a
// leaf package (no internal/store or internal/ledger imports) so both can
// depend on it without an import cycle (#1200 review: ledger.NewPGStoreFromURL
// was a fourth postgres dialector missed by the first retry pass).
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

// RetryAttempts/RetryBackoff absorb a short DNS/dial blip (#1193 measured
// Docker's embedded DNS failing to resolve quack-postgres for ~20s every 30min)
// without turning it into a run failure. Only the TCP dial retries here - a
// connected query error is never retried.
const RetryAttempts = 3

var RetryBackoff = 2 * time.Second

// withDialRetry wraps a dial func so a transient failure (DNS, connection
// refused) gets a couple of short retries before giving up. Split out from
// retryingDialFunc so a test can inject a fake dial instead of a real net.Dialer.
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

// Open is the single construction point for every postgres GORM dialector
// (main store, session service, artifact service, ledger store) - dial
// retries apply once here rather than at each call site.
func Open(url string) (gorm.Dialector, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.DialFunc = retryingDialFunc()
	sqlDB := stdlib.OpenDB(*cfg)
	return postgres.New(postgres.Config{Conn: sqlDB}), nil
}

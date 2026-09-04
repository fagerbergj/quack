package store

import (
	"context"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// dialRetryAttempts/dialRetryBackoff absorb a short DNS/dial blip (#1193 measured
// Docker's embedded DNS failing to resolve quack-postgres for ~20s every 30min)
// without turning it into a run failure. Only the TCP dial retries here - a
// connected query error is never retried.
const dialRetryAttempts = 3

var dialRetryBackoff = 2 * time.Second

// withDialRetry wraps a dial func so a transient failure (DNS, connection
// refused) gets a couple of short retries before giving up. Split out from
// retryingDialFunc so a test can inject a fake dial instead of a real net.Dialer.
func withDialRetry(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var lastErr error
		for attempt := 1; attempt <= dialRetryAttempts; attempt++ {
			conn, err := dial(ctx, network, addr)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if attempt == dialRetryAttempts {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(dialRetryBackoff):
			}
		}
		return nil, lastErr
	}
}

func retryingDialFunc() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{}
	return withDialRetry(d.DialContext)
}

// openPostgres is the single construction point for every postgres GORM
// dialector (main store, session service, artifact service) - dial retries
// apply once here rather than at each call site.
func openPostgres(url string) (gorm.Dialector, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.DialFunc = retryingDialFunc()
	sqlDB := stdlib.OpenDB(*cfg)
	return postgres.New(postgres.Config{Conn: sqlDB}), nil
}

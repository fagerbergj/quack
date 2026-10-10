package serve

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/fagerbergj/quack/internal/cli"
)

// Managed runs an embedded stores-only compose (Postgres + Qdrant) via `docker compose -p quack-stores`.
// ponytail: shells to docker compose rather than reimplementing orchestration; one stack per machine.

//go:embed stores.compose.yml
var storesCompose []byte

// storesProject isolates the managed stores stack from the repo's dev
// docker-compose.yml and any other compose stacks on the machine.
const storesProject = "quack-stores"

// stateDir is where quack keeps machine-local state (the embedded stores compose
// file). Lives under the CLI home (~/.quack or $QUACK_HOME).
func stateDir() string { return cli.Home() }

// composePath is stable so up and down reference the same file.
func composePath() string { return filepath.Join(stateDir(), "stores.compose.yml") }

// writeStoresCompose extracts the embedded compose to disk (docker needs a file
// path, not stdin). Idempotent.
func writeStoresCompose() error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	return os.WriteFile(composePath(), storesCompose, 0o644)
}

// upStores: `up -d --wait` waits for Postgres's pg_isready healthcheck; qdrant has none, so it is
// gated by a TCP poll. Idempotent.
func upStores(ctx context.Context) error {
	if err := writeStoresCompose(); err != nil {
		return err
	}
	slog.Info("managed topology: bringing up stores", "component", "serve", "project", storesProject)
	if out, err := runCompose(ctx, "up", "-d", "--wait"); err != nil {
		return fmt.Errorf("docker compose up: %w\n%s", err, out)
	}
	if err := waitListeningCtx(ctx, ":6334", 60*time.Second); err != nil {
		return fmt.Errorf("managed stores did not become ready: %w (is docker running?)", err)
	}
	slog.Info("managed topology: stores ready", "component", "serve")
	return nil
}

// runCompose returns combined output so callers can surface it on error.
func runCompose(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"compose", "-p", storesProject, "-f", composePath()}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	return cmd.CombinedOutput()
}

// listening reports whether addr currently accepts a TCP connection.
func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", dialAddr(addr), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// dialAddr makes a listen address (":8080") dialable ("127.0.0.1:8080").
func dialAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}

// waitListeningCtx blocks until addr accepts a TCP connection or ctx/deadline.
// Reuses the dialAddr normalisation (":5432" → "127.0.0.1:5432").
func waitListeningCtx(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if listening(addr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for %s", dialAddr(addr))
}

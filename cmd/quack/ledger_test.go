package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/pgtest"
)

// buildRecovererOrWarn degrades a build failure to nil plus a stderr warning instead of aborting,
// so a misconfigured extension can't hide orphans.
func TestBuildRecovererOrWarn_ConfigLoadFailureDegrades(t *testing.T) {
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	var errBuf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errBuf)

	rec := buildRecovererOrWarn(cmd, false)
	if rec != nil {
		t.Fatalf("recoverer = %v, want nil on a config load failure", rec)
	}
	if !strings.Contains(errBuf.String(), "continuing without a recoverer") {
		t.Errorf("stderr = %q, want a warning that recovery continues without a recoverer", errBuf.String())
	}
}

func TestBuildRecovererOrWarn_DryRunSkipsBuild(t *testing.T) {
	// A bad QUACK_CONFIG would make a non-dry-run call warn; --dry-run must
	// never even attempt the build (and so never warn).
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	var errBuf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errBuf)

	rec := buildRecovererOrWarn(cmd, true)
	if rec != nil {
		t.Fatalf("recoverer = %v, want nil under --dry-run", rec)
	}
	if errBuf.String() != "" {
		t.Errorf("stderr = %q, want no output under --dry-run", errBuf.String())
	}
}

// Flag-registration only: their JSON shape is proven directly against
// RunLedgerRebuild/RunLedgerRecover's output in internal/cli.
func TestLedgerRebuildRecover_JSONFlagRegistered(t *testing.T) {
	if newLedgerRebuildCmd().Flags().Lookup("json") == nil {
		t.Error("ledger rebuild is missing --json")
	}
	if newLedgerRecoverCmd().Flags().Lookup("json") == nil {
		t.Error("ledger recover is missing --json")
	}
}

// Read-only ledger commands open the session, recording and artifact stores without migrating, so a CLI of
// another version never alters the schema it reads.
func TestOpenLedgerAndStores_ReadOnlyRunsNoDDL(t *testing.T) {
	dsn := pgtest.DSN(t)
	cfgPath := filepath.Join(t.TempDir(), "quack.yaml")
	cfg := `
providers: { p: { kind: openai, endpoint: "http://localhost:1", api_key: x } }
orchestrator: { provider: p, model: m }
models: { m: { provider: p, role: worker } }
stores: { main: { kind: postgres, url: "` + dsn + `" } }
session: { store: main }
artifacts: { store: main }
observability: { recording: { observations: true, store: main } }
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUACK_CONFIG", cfgPath)

	if _, _, _, err := openLedgerAndStores(&cobra.Command{}, false); err != nil {
		t.Fatalf("openLedgerAndStores: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("read-only open created %d table(s), want 0", tables)
	}
}

// ledger list must fail fast without a server rather than boot an in-process engine and its boot-time writes.
func TestLedgerList_NoServerRefusesInProcessBoot(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	cmd := newLedgerListCmd()
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "never boots one in-process") {
		t.Fatalf("err = %v, want the needs-a-running-server error", err)
	}
}

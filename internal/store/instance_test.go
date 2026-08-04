package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"

	"github.com/fagerbergj/quack/internal/dag"
)

// TestFailStaleDagNodes_DoesNotTouchLiveInstanceNodes is issue #683's core
// repro: a second Store opened against the same database (a CLI subcommand's
// startup, sharing QUACK_DATABASE_URL with a running server) must not fail a
// node a live instance owns, even without #683's other fix (InProcess never
// calling FailStaleDagNodes at all) - this proves the reconciliation query
// itself is safe.
func TestFailStaleDagNodes_DoesNotTouchLiveInstanceNodes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	ctx := context.Background()

	server, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (server): %v", err)
	}
	server.SetInstanceID("live-server")
	if err := server.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusRunning), InstanceID: server.InstanceID()}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	cli, err := New("sqlite", dbPath) // fresh random instance id, like a CLI bootstrap
	if err != nil {
		t.Fatalf("New (cli): %v", err)
	}
	n, err := cli.FailStaleDagNodes(ctx)
	if err != nil {
		t.Fatalf("FailStaleDagNodes: %v", err)
	}
	if n != 0 {
		t.Fatalf("CLI reconciliation touched %d nodes, want 0", n)
	}

	got, err := server.GetDagNode(ctx, "p1", "n1")
	if err != nil || got == nil || got.Status != string(dag.StatusRunning) {
		t.Fatalf("live node status = %+v err=%v, want running", got, err)
	}
}

// TestFailStaleDagNodes_ReconcilesOwnPriorIncarnation proves a real server
// restart (same persisted instance id from LoadOrCreateInstanceID) still
// cleans up what it left running/queued before it died.
func TestFailStaleDagNodes_ReconcilesOwnPriorIncarnation(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "quack.db")
	stateDir := filepath.Join(tmp, "workspace")
	ctx := context.Background()

	id1, err := LoadOrCreateInstanceID(stateDir)
	if err != nil {
		t.Fatalf("LoadOrCreateInstanceID: %v", err)
	}
	first, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (first): %v", err)
	}
	first.SetInstanceID(id1)
	if err := first.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusRunning), InstanceID: id1}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	// "first" crashes here - no cleanup, node stays running.

	id2, err := LoadOrCreateInstanceID(stateDir) // same dir: same deployment restarting
	if err != nil {
		t.Fatalf("LoadOrCreateInstanceID (restart): %v", err)
	}
	if id2 != id1 {
		t.Fatalf("persisted instance id changed across restart: %q vs %q", id1, id2)
	}
	second, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}
	second.SetInstanceID(id2)
	n, err := second.FailStaleDagNodes(ctx)
	if err != nil {
		t.Fatalf("FailStaleDagNodes: %v", err)
	}
	if n != 1 {
		t.Fatalf("restart reconciliation touched %d nodes, want 1", n)
	}
	got, err := second.GetDagNode(ctx, "p1", "n1")
	if err != nil || got == nil || got.Status != string(dag.StatusFailed) {
		t.Fatalf("node status = %+v err=%v, want failed", got, err)
	}
}

// TestFailStaleDagNodes_ConcurrentServersDontFailEachOther covers two
// separately-deployed servers (distinct persisted instance ids) sharing one
// database: reconciling one's startup must never touch the other's live row.
func TestFailStaleDagNodes_ConcurrentServersDontFailEachOther(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	ctx := context.Background()

	idA, err := LoadOrCreateInstanceID(filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatalf("LoadOrCreateInstanceID a: %v", err)
	}
	idB, err := LoadOrCreateInstanceID(filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatalf("LoadOrCreateInstanceID b: %v", err)
	}

	a, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (a): %v", err)
	}
	a.SetInstanceID(idA)
	if err := a.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusRunning), InstanceID: idA}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	b, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (b): %v", err)
	}
	b.SetInstanceID(idB)
	n, err := b.FailStaleDagNodes(ctx)
	if err != nil {
		t.Fatalf("FailStaleDagNodes: %v", err)
	}
	if n != 0 {
		t.Fatalf("server B's startup touched %d of server A's nodes, want 0", n)
	}
	got, err := a.GetDagNode(ctx, "p1", "n1")
	if err != nil || got == nil || got.Status != string(dag.StatusRunning) {
		t.Fatalf("server A's node status = %+v err=%v, want running", got, err)
	}
}

// TestFailStaleDagNodes_ReconcilesPreMigrationRows proves an existing
// database upgrades cleanly: a row written before InstanceID existed (empty
// column, simulated here by inserting outside UpsertDagNode's stamping)
// must not be read as "belongs to some other instance" and become immortal.
func TestFailStaleDagNodes_ReconcilesPreMigrationRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	ctx := context.Background()

	st, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := st.db.Exec(`INSERT INTO dag_nodes (node_id, plan_id, status) VALUES (?, ?, ?)`,
		"n1", "p1", string(dag.StatusQueued)).Error; err != nil {
		t.Fatalf("simulate pre-migration row: %v", err)
	}

	other, err := New("sqlite", dbPath) // a totally different, unrelated instance
	if err != nil {
		t.Fatalf("New (other): %v", err)
	}
	n, err := other.FailStaleDagNodes(ctx)
	if err != nil {
		t.Fatalf("FailStaleDagNodes: %v", err)
	}
	if n != 1 {
		t.Fatalf("pre-migration row reconciliation touched %d nodes, want 1", n)
	}
}

// TestFailStaleDagNodes_MigratesExistingDatabaseCleanly is the literal
// upgrade case: a dag_nodes table that predates the InstanceID/UpdatedAt
// columns, with a row already stuck in "running". AutoMigrate must add the
// columns without erroring on the existing row, and that row must not
// become unreconcilable (immortal) just because it belongs to nobody.
func TestFailStaleDagNodes_MigratesExistingDatabaseCleanly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	ctx := context.Background()

	// Build the pre-#683 table shape directly - no instance_id, no updated_at.
	raw, err := sql.Open(sqlite.DriverName, dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE dag_nodes (
		node_id TEXT NOT NULL, plan_id TEXT NOT NULL, status TEXT,
		PRIMARY KEY (node_id, plan_id))`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO dag_nodes (node_id, plan_id, status) VALUES (?, ?, ?)`,
		"n1", "p1", string(dag.StatusRunning)); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	st, err := New("sqlite", dbPath) // runs AutoMigrate against the legacy table
	if err != nil {
		t.Fatalf("New (migrate): %v", err)
	}
	if _, err := st.GetDagNode(ctx, "p1", "n1"); err != nil {
		t.Fatalf("reading a migrated legacy row: %v", err)
	}
	n, err := st.FailStaleDagNodes(ctx)
	if err != nil {
		t.Fatalf("FailStaleDagNodes: %v", err)
	}
	if n != 1 {
		t.Fatalf("legacy row reconciliation touched %d nodes, want 1 (would be immortal otherwise)", n)
	}
}

// TestFailStaleDagNodes_StaleNodeCeilingCatchesPermanentOrphan is the
// dead-man's-switch: a node whose owning instance never comes back (its
// persisted identity file lost, e.g. a fresh volume) must still get cleaned
// up eventually rather than staying in-flight forever.
func TestFailStaleDagNodes_StaleNodeCeilingCatchesPermanentOrphan(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	ctx := context.Background()

	st, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusRunning), InstanceID: "long-gone-instance"}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	old := time.Now().UTC().Add(-2 * staleNodeCeiling)
	if err := st.db.Exec(`UPDATE dag_nodes SET updated_at = ? WHERE node_id = ? AND plan_id = ?`, old, "n1", "p1").Error; err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}

	reconciler, err := New("sqlite", dbPath) // fresh id, never matches "long-gone-instance"
	if err != nil {
		t.Fatalf("New (reconciler): %v", err)
	}
	n, err := reconciler.FailStaleDagNodes(ctx)
	if err != nil {
		t.Fatalf("FailStaleDagNodes: %v", err)
	}
	if n != 1 {
		t.Fatalf("permanent orphan not reconciled past staleNodeCeiling: got %d", n)
	}
}

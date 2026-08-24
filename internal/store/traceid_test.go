package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

// A retry re-runs a node that already completed with a trace id from the
// earlier run. node_start owns the column, so an empty id from a span-less
// retry must CLEAR the stale one - a preserved id renders a deep link to the
// wrong execution, which reads as correct in the UI.
func TestUpsertDagNode_NodeStartOwnsTraceID(t *testing.T) {
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}

	start := func(trace string) {
		t.Helper()
		if err := st.UpsertDagNode(ctx, DagNode{
			NodeID: "n1", PlanID: "p1", Status: string(dag.StatusRunning),
			TraceID: trace, TraceIDSet: true,
		}); err != nil {
			t.Fatalf("UpsertDagNode(start %q): %v", trace, err)
		}
	}
	traceID := func() string {
		t.Helper()
		n, err := st.GetDagNode(ctx, "p1", "n1")
		if err != nil || n == nil {
			t.Fatalf("GetDagNode: %v", err)
		}
		return n.TraceID
	}

	start("trace-from-first-run")
	if got := traceID(); got != "trace-from-first-run" {
		t.Fatalf("after first node_start: trace_id = %q, want the first run's id", got)
	}

	// node_done carries no trace id and must not blank it.
	if err := st.UpsertDagNode(ctx, DagNode{
		NodeID: "n1", PlanID: "p1", Status: string(dag.StatusDone), Output: "hi",
	}); err != nil {
		t.Fatalf("UpsertDagNode(done): %v", err)
	}
	if got := traceID(); got != "trace-from-first-run" {
		t.Fatalf("after node_done: trace_id = %q, want it preserved", got)
	}

	// The retry: otel disabled or no span on the retry path, so node_start
	// carries "". The stale id must not survive.
	start("")
	if got := traceID(); got != "" {
		t.Fatalf("after retry node_start with no trace: trace_id = %q, want it cleared (stale id links to the previous run)", got)
	}

	start("trace-from-third-run")
	if got := traceID(); got != "trace-from-third-run" {
		t.Fatalf("after third node_start: trace_id = %q, want the new run's id", got)
	}
}

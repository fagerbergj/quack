package dag

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// TestSaveDagPlanRecord covers #1095: an accepted plan writes "dag_plan:main",
// its content validates against the registered dag_plan schema, and it shows
// up in a list_artifacts-style listing.
func TestSaveDagPlanRecord(t *testing.T) {
	svc := artifact.InMemoryService()
	p := Plan{
		ID:          "plan-1",
		Nodes:       []Node{{ID: "n1", AgentName: "web-researcher", Task: "look something up"}},
		UserMessage: "please research this",
	}

	id, rev, err := SaveDagPlanRecord(context.Background(), svc, "quack", "u1", "chat1", "turn1", p)
	if err != nil {
		t.Fatalf("SaveDagPlanRecord: %v", err)
	}
	if id != "dag_plan:main" {
		t.Fatalf("id = %q, want %q", id, "dag_plan:main")
	}
	if rev != 1 {
		t.Fatalf("revision = %d, want 1", rev)
	}

	c := recordstore.New(svc, "quack", "u1", "chat1")
	raw, gotRev, ok, err := c.Latest(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if gotRev != 1 {
		t.Fatalf("Latest revision = %d, want 1", gotRev)
	}
	var loaded Plan
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("stored content doesn't unmarshal into Plan: %v", err)
	}
	if loaded.ID != p.ID || len(loaded.Nodes) != 1 || loaded.Nodes[0].AgentName != "web-researcher" {
		t.Fatalf("stored plan = %+v, want a round-trip of %+v", loaded, p)
	}

	spec, ok := recordstore.SpecFor(kindDagPlan)
	if !ok {
		t.Fatal("dag_plan kind not registered")
	}
	if err := spec.Validate(raw); err != nil {
		t.Fatalf("stored dag_plan fails its own registered validator: %v", err)
	}

	summaries, err := c.List(context.Background(), kindDagPlan)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].ID != id {
		t.Fatalf("List(dag_plan) = %+v, want exactly [%s]", summaries, id)
	}
}

// TestSaveDagPlanRecord_NoArtifactServiceFailsOpen covers the pre-#1090 case
// (no artifact service configured): the caller Warn-logs and moves on, never
// blocking plan acceptance.
func TestSaveDagPlanRecord_NoArtifactServiceFailsOpen(t *testing.T) {
	if _, _, err := SaveDagPlanRecord(context.Background(), nil, "quack", "u1", "chat1", "turn1", Plan{ID: "p"}); err == nil {
		t.Fatal("want an error with nil artifacts, so the caller knows to skip rather than silently write to nothing")
	}
}

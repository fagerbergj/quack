package dag

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// TestSaveDagPlanRecord covers #1095: a plan writes "dag_plan:main", its
// content validates against the registered dag_plan schema, and it shows up
// in a list_artifacts-style listing.
func TestSaveDagPlanRecord(t *testing.T) {
	svc := artifact.InMemoryService()
	rec := DagPlanRecord{
		PlanID:      "plan-1",
		Assignments: []Assignment{{NodeID: "web-researcher-1", Task: "look something up"}},
	}

	id, rev, err := SaveDagPlanRecord(context.Background(), svc, "quack", "u1", "chat1", "turn1", rec)
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
	var loaded DagPlanRecord
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("stored content doesn't unmarshal into DagPlanRecord: %v", err)
	}
	if loaded.PlanID != rec.PlanID || len(loaded.Assignments) != 1 || loaded.Assignments[0].NodeID != "web-researcher-1" {
		t.Fatalf("stored plan = %+v, want a round-trip of %+v", loaded, rec)
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
	rec := DagPlanRecord{PlanID: "p", Assignments: []Assignment{{NodeID: "n1", Task: "x"}}}
	if _, _, err := SaveDagPlanRecord(context.Background(), nil, "quack", "u1", "chat1", "turn1", rec); err == nil {
		t.Fatal("want an error with nil artifacts, so the caller knows to skip rather than silently write to nothing")
	}
}

// TestLoadDagPlanRecord_NoneYet covers the ok=false path - a chat that has
// never called create_plan.
func TestLoadDagPlanRecord_NoneYet(t *testing.T) {
	svc := artifact.InMemoryService()
	_, _, ok, err := LoadDagPlanRecord(context.Background(), svc, "quack", "u1", "chat1")
	if err != nil {
		t.Fatalf("LoadDagPlanRecord: %v", err)
	}
	if ok {
		t.Fatal("want ok=false for a chat with no dag_plan record yet")
	}
}

// TestLoadDagPlanRecord_RoundTrip covers reading back what SaveDagPlanRecord wrote.
func TestLoadDagPlanRecord_RoundTrip(t *testing.T) {
	svc := artifact.InMemoryService()
	rec := DagPlanRecord{PlanID: "plan-2", Assignments: []Assignment{{NodeID: "n1", Task: "x"}}}
	if _, _, err := SaveDagPlanRecord(context.Background(), svc, "quack", "u1", "chat1", "turn1", rec); err != nil {
		t.Fatalf("SaveDagPlanRecord: %v", err)
	}
	loaded, rev, ok, err := LoadDagPlanRecord(context.Background(), svc, "quack", "u1", "chat1")
	if err != nil || !ok {
		t.Fatalf("LoadDagPlanRecord: ok=%v err=%v", ok, err)
	}
	if rev != 1 || loaded.PlanID != rec.PlanID {
		t.Fatalf("loaded = %+v rev=%d, want %+v rev=1", loaded, rev, rec)
	}
}

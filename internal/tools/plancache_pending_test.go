package tools

import (
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

// A plan that was created but never executed is PENDING: the turn did no work.
//
// The live failure this pins: the orchestrator called `plan`, read the tool's
// "review before executing" summary, replied "The plan is solid - 4 parallel
// code-explorer nodes researching each project's actual source code…" and then
// finished the turn WITHOUT calling `execute`. It described running the work
// instead of running it. Because it had emitted text, the turn looked "produced",
// no continuation fired, and the whole run ended having executed nothing.
func TestPlanCache_PendingWhenPlannedButNotExecuted(t *testing.T) {
	c := NewPlanCache()

	if _, pending := c.Pending(); pending {
		t.Fatal("a fresh cache has no plan; Pending must be false")
	}

	c.Put(dag.Plan{ID: "p1"})

	id, pending := c.Pending()
	if !pending {
		t.Fatal("a plan was created but never executed; Pending must be true (the turn did NO work)")
	}
	if id != "p1" {
		t.Fatalf("Pending returned %q; want the pending plan id p1", id)
	}

	// Executing it clears the pending state.
	c.SetSelected("p1")
	if _, pending := c.Pending(); pending {
		t.Fatal("the plan was executed; Pending must be false")
	}
}

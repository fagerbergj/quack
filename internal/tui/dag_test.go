package tui

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/stream"
)

func sampleDAG() *dagState {
	return newDAG(stream.DagPlanData{
		PlanID: "p",
		Nodes: []stream.DagNodeDef{
			{ID: "a", Agent: "researcher", Task: "search the web"},
			{ID: "b", Agent: "researcher", Task: "search more", DependsOn: []string{"a"}},
			{ID: "c", Agent: "synthesizer", Task: "combine", DependsOn: []string{"a", "b"}},
		},
	})
}

func TestDAG_DepthLayers(t *testing.T) {
	d := sampleDAG()
	memo := map[string]int{}
	if got := d.depth("a", memo); got != 0 {
		t.Errorf("root depth = %d, want 0", got)
	}
	if got := d.depth("b", memo); got != 1 {
		t.Errorf("b depth = %d, want 1", got)
	}
	if got := d.depth("c", memo); got != 2 { // a→b→c is the longest chain
		t.Errorf("c depth = %d, want 2", got)
	}
}

func TestDAG_DepthHandlesCycle(t *testing.T) {
	// A malformed plan with a cycle must not infinite-loop.
	d := newDAG(stream.DagPlanData{Nodes: []stream.DagNodeDef{
		{ID: "x", DependsOn: []string{"y"}},
		{ID: "y", DependsOn: []string{"x"}},
	}})
	_ = d.depth("x", map[string]int{}) // must terminate
}

func TestDAG_StatusAndCounts(t *testing.T) {
	d := sampleDAG()
	d.set("a", statusDone)
	d.set("b", statusRunning)
	d.fail("c", "judge rejected")
	done, failed, total := d.counts()
	if done != 1 || failed != 1 || total != 3 {
		t.Errorf("counts = %d/%d/%d, want 1/1/3", done, failed, total)
	}
	if d.nodes[2].failErr != "judge rejected" {
		t.Errorf("fail must record the error: %q", d.nodes[2].failErr)
	}
}

func TestDAG_RenderHasIconsAndLabels(t *testing.T) {
	d := sampleDAG()
	d.set("a", statusDone)
	d.set("b", statusRunning)
	out := d.render("•", 200)
	for _, want := range []string{"✓", "researcher", "synthesizer", "1/3 done"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestDAG_RenderUnknownNodeNoop(t *testing.T) {
	d := sampleDAG()
	d.set("ghost", statusDone) // not in the plan → ignored, no panic
	if done, _, _ := d.counts(); done != 0 {
		t.Errorf("setting an unknown node must not change counts, done=%d", done)
	}
}

package dag

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

// writableGateCfg mimics a real code-implementer's startup-time config
// (internal/serve/serve.go's perAgentGateCfg): ACP-backed, writable, with a
// delivery target wired - the shape a plan run's cfgFor hands back
// regardless of what the run itself asked for.
func writableGateCfg() vetting.Config {
	return vetting.Config{
		ExternalWorker: true,
		ReadOnly:       false,
		Deliver: func(context.Context, vetting.DeliveryContext) ([]vetting.DeliveryItemOutcome, error) {
			return nil, nil
		},
	}
}

// prNode mirrors newGatedNode's own formula (graph.go) for whether a node
// gets offered stage_pr/stage_push.
func prNode(cfg vetting.Config) bool {
	return cfg.ExternalWorker && !cfg.ReadOnly && cfg.Deliver != nil
}

// TestPlanOnlyForcesReadOnlyNoDeliver pins #739 test case 1: every node of a
// planOnly plan comes out read-only with a nil deliver target, whatever its
// own agent's base config says - asserted on the constructed config, not
// model output.
func TestPlanOnlyForcesReadOnlyNoDeliver(t *testing.T) {
	plan := Plan{PlanOnly: true, Nodes: []Node{
		{ID: "n1", AgentName: implementerAgent},
		{ID: "n2", AgentName: reviewerAgent},
		{ID: "n3", AgentName: explorerAgent},
	}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	for _, n := range plan.Nodes {
		cfg := nodeGateConfig(plan, n, nil, cfgFor, "chat1")
		if !cfg.ReadOnly {
			t.Errorf("node %q (%s): ReadOnly = false, want true for a planOnly plan", n.ID, n.AgentName)
		}
		if cfg.Deliver != nil {
			t.Errorf("node %q (%s): Deliver is set, want nil for a planOnly plan", n.ID, n.AgentName)
		}
	}
}

// TestPlanOnlyOffersNoWritableNode pins #739 test case 2: prNode - the exact
// gate newGatedNode uses to decide whether stage_pr/stage_push is registered
// on the node's MCP server (internal/acp/memorymcp.go's `sess.PRStage !=
// nil`) - is false for every node of a planOnly plan.
func TestPlanOnlyOffersNoWritableNode(t *testing.T) {
	plan := Plan{PlanOnly: true, Nodes: []Node{
		{ID: "n1", AgentName: implementerAgent},
		{ID: "n2", AgentName: reviewerAgent},
		{ID: "n3", AgentName: explorerAgent},
	}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	for _, n := range plan.Nodes {
		cfg := nodeGateConfig(plan, n, nil, cfgFor, "chat1")
		if prNode(cfg) {
			t.Errorf("node %q (%s): prNode = true, stage_pr would be registered on a planOnly run", n.ID, n.AgentName)
		}
	}
}

// TestNonPlanRunKeepsWritableNode pins #739 test case 3: an ordinary
// (non-planOnly) run is unchanged - writable node, deliver target present,
// stage_pr offered.
func TestNonPlanRunKeepsWritableNode(t *testing.T) {
	plan := Plan{Nodes: []Node{{ID: "n1", AgentName: implementerAgent}}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1")

	if cfg.ReadOnly {
		t.Error("ReadOnly = true, want false for a non-planOnly run")
	}
	if cfg.Deliver == nil {
		t.Error("Deliver is nil, want the agent's own deliver target for a non-planOnly run")
	}
	if !prNode(cfg) {
		t.Error("prNode = false, want true - a non-planOnly writable node must still get stage_pr offered")
	}
}

// TestPlanOnlyImplementerNodeHasNoWritableCapability pins #739 test case 4 -
// the document-pipeline#124 case verbatim: a planOnly plan whose planner
// named code-implementer must still produce no writable node, even though
// code-implementer's own base config (cfgFor) is fully writable. This is the
// case that fails against pre-#739 main, where buildGateNodes read cfgFor's
// result straight through with no plan.PlanOnly check at all.
func TestPlanOnlyImplementerNodeHasNoWritableCapability(t *testing.T) {
	plan := Plan{PlanOnly: true, Nodes: []Node{{ID: "n1", AgentName: implementerAgent}}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1")

	if prNode(cfg) {
		t.Fatal("a planOnly run's code-implementer node has prNode = true - stage_pr would be offered, reproducing document-pipeline#124")
	}
	if !cfg.ReadOnly || cfg.Deliver != nil {
		t.Fatalf("cfg = %+v, want ReadOnly=true and Deliver=nil for a planOnly code-implementer node", cfg)
	}
}

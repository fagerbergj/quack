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
		cfg := nodeGateConfig(plan, n, nil, cfgFor, "chat1", "")
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
		cfg := nodeGateConfig(plan, n, nil, cfgFor, "chat1", "")
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
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

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
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

	if prNode(cfg) {
		t.Fatal("a planOnly run's code-implementer node has prNode = true - stage_pr would be offered, reproducing document-pipeline#124")
	}
	if !cfg.ReadOnly || cfg.Deliver != nil {
		t.Fatalf("cfg = %+v, want ReadOnly=true and Deliver=nil for a planOnly code-implementer node", cfg)
	}
}

// TestNodeGateConfig_CarriesSource pins the token-metrics attribution
// plumbing: nodeGateConfig's source parameter (extracted from the run's
// ledger coords by RunPlanAsGraph/RetryPlanInNode, before any RunNode
// scheduling) must land on cfg.Source, exactly like chatID lands on cfg.ChatID.
func TestNodeGateConfig_CarriesSource(t *testing.T) {
	plan := Plan{Nodes: []Node{{ID: "n1", AgentName: implementerAgent}}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1", "github")

	if cfg.Source != "github" {
		t.Errorf("cfg.Source = %q, want %q", cfg.Source, "github")
	}
	if cfg.ChatID != "chat1" {
		t.Errorf("cfg.ChatID = %q, want %q", cfg.ChatID, "chat1")
	}
}

// TestReviewPlanWiresSynthesizerIntoFanout pins the #965 wiring: in a
// two-reviewer + synthesizer plan, all three nodes share the run's
// ReviewFanout, so the reviewers stage without delivering and the
// synthesizer's answer becomes the one submitted review.
func TestReviewPlanWiresSynthesizerIntoFanout(t *testing.T) {
	plan := Plan{ID: t.Name(), Nodes: []Node{
		{ID: "review-backend", AgentName: reviewerAgent},
		{ID: "review-frontend", AgentName: reviewerAgent},
		{ID: "synthesize", AgentName: synthesizerAgent, DependsOn: []string{"review-backend", "review-frontend"}},
	}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	var fanouts []*vetting.ReviewFanout
	for _, n := range plan.Nodes {
		cfg := nodeGateConfig(plan, n, nil, cfgFor, "chat1", "")
		if cfg.ReviewFanout == nil {
			t.Fatalf("node %s: ReviewFanout = nil, want the shared fan-in", n.ID)
		}
		fanouts = append(fanouts, cfg.ReviewFanout)
	}
	if fanouts[0] != fanouts[1] || fanouts[1] != fanouts[2] {
		t.Fatal("nodes got different fan-ins, want one shared per plan")
	}
}

// Without a synthesizer node, reviewer-only plans keep the #867 behavior and
// non-reviewer nodes stay out of the fan-in.
func TestReviewPlanWithoutSynthesizerKeepsReviewerOnlyFanout(t *testing.T) {
	plan := Plan{ID: t.Name(), Nodes: []Node{
		{ID: "r1", AgentName: reviewerAgent},
		{ID: "r2", AgentName: reviewerAgent},
		{ID: "explore", AgentName: explorerAgent},
	}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	if cfg := nodeGateConfig(plan, plan.Nodes[2], nil, cfgFor, "chat1", ""); cfg.ReviewFanout != nil {
		t.Fatal("explorer node got a ReviewFanout, want nil")
	}
	if cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1", ""); cfg.ReviewFanout == nil {
		t.Fatal("reviewer node missing its ReviewFanout")
	}
}

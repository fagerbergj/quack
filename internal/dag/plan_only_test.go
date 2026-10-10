package dag

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

// writableGateCfg mimics serve's perAgentGateCfg for a code-implementer: ACP-backed,
// writable, delivery target wired.
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

// Every node of a planOnly plan is read-only with a nil deliver target, whatever its
// agent's base config says.
func TestPlanOnlyForcesReadOnlyNoDeliver(t *testing.T) {
	plan := Plan{PlanOnly: true, Nodes: []Node{
		{ID: "n1", AgentName: implementerAgent},
		{ID: "n2", AgentName: reviewerAgent},
		{ID: "n3", AgentName: explorerAgent},
	}}
	cfgFor := func(context.Context, string) vetting.Config { return writableGateCfg() }
	for _, n := range plan.Nodes {
		cfg := nodeGateConfig(context.Background(), plan, n, nil, cfgFor, "chat1", "")
		if !cfg.ReadOnly {
			t.Errorf("node %q (%s): ReadOnly = false, want true for a planOnly plan", n.ID, n.AgentName)
		}
		if cfg.Deliver != nil {
			t.Errorf("node %q (%s): Deliver is set, want nil for a planOnly plan", n.ID, n.AgentName)
		}
	}
}

// prNode, which decides whether stage_pr/stage_push is offered, is false for every
// planOnly node.
func TestPlanOnlyOffersNoWritableNode(t *testing.T) {
	plan := Plan{PlanOnly: true, Nodes: []Node{
		{ID: "n1", AgentName: implementerAgent},
		{ID: "n2", AgentName: reviewerAgent},
		{ID: "n3", AgentName: explorerAgent},
	}}
	cfgFor := func(context.Context, string) vetting.Config { return writableGateCfg() }
	for _, n := range plan.Nodes {
		cfg := nodeGateConfig(context.Background(), plan, n, nil, cfgFor, "chat1", "")
		if prNode(cfg) {
			t.Errorf("node %q (%s): prNode = true, stage_pr would be registered on a planOnly run", n.ID, n.AgentName)
		}
	}
}

// A non-planOnly run keeps the writable node, deliver target, and stage_pr.
func TestNonPlanRunKeepsWritableNode(t *testing.T) {
	plan := Plan{Nodes: []Node{{ID: "n1", AgentName: implementerAgent}}}
	cfgFor := func(context.Context, string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

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

// A planOnly plan naming code-implementer still yields no writable node, though the
// agent's own cfgFor is fully writable.
func TestPlanOnlyImplementerNodeHasNoWritableCapability(t *testing.T) {
	plan := Plan{PlanOnly: true, Nodes: []Node{{ID: "n1", AgentName: implementerAgent}}}
	cfgFor := func(context.Context, string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[0], nil, cfgFor, "chat1", "")

	if prNode(cfg) {
		t.Fatal("a planOnly run's code-implementer node has prNode = true - stage_pr would be offered, reproducing document-pipeline#124")
	}
	if !cfg.ReadOnly || cfg.Deliver != nil {
		t.Fatalf("cfg = %+v, want ReadOnly=true and Deliver=nil for a planOnly code-implementer node", cfg)
	}
}

// nodeGateConfig's source param lands on cfg.Source, like chatID on cfg.ChatID.
func TestNodeGateConfig_CarriesSource(t *testing.T) {
	plan := Plan{Nodes: []Node{{ID: "n1", AgentName: implementerAgent}}}
	cfgFor := func(context.Context, string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[0], nil, cfgFor, "chat1", "github")

	if cfg.Source != "github" {
		t.Errorf("cfg.Source = %q, want %q", cfg.Source, "github")
	}
	if cfg.ChatID != "chat1" {
		t.Errorf("cfg.ChatID = %q, want %q", cfg.ChatID, "chat1")
	}
}

// Reviewers and synthesizer share the run's ReviewFanout, so reviewers stage without
// delivering and the synthesizer's answer is the one submitted review.
func TestReviewPlanWiresSynthesizerIntoFanout(t *testing.T) {
	plan := Plan{ID: t.Name(), Nodes: []Node{
		{ID: "review-backend", AgentName: reviewerAgent},
		{ID: "review-frontend", AgentName: reviewerAgent},
		{ID: "synthesize", AgentName: synthesizerAgent, DependsOn: []string{"review-backend", "review-frontend"}},
	}}
	cfgFor := func(context.Context, string) vetting.Config { return writableGateCfg() }
	var fanouts []*vetting.ReviewFanout
	for _, n := range plan.Nodes {
		cfg := nodeGateConfig(context.Background(), plan, n, nil, cfgFor, "chat1", "")
		if cfg.ReviewFanout == nil {
			t.Fatalf("node %s: ReviewFanout = nil, want the shared fan-in", n.ID)
		}
		fanouts = append(fanouts, cfg.ReviewFanout)
	}
	if fanouts[0] != fanouts[1] || fanouts[1] != fanouts[2] {
		t.Fatal("nodes got different fan-ins, want one shared per plan")
	}
}

// Without a synthesizer, reviewer-only plans keep reviewer-only fan-in and non-reviewer
// nodes stay out of it.
func TestReviewPlanWithoutSynthesizerKeepsReviewerOnlyFanout(t *testing.T) {
	plan := Plan{ID: t.Name(), Nodes: []Node{
		{ID: "r1", AgentName: reviewerAgent},
		{ID: "r2", AgentName: reviewerAgent},
		{ID: "explore", AgentName: explorerAgent},
	}}
	cfgFor := func(context.Context, string) vetting.Config { return writableGateCfg() }
	if cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[2], nil, cfgFor, "chat1", ""); cfg.ReviewFanout != nil {
		t.Fatal("explorer node got a ReviewFanout, want nil")
	}
	if cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[0], nil, cfgFor, "chat1", ""); cfg.ReviewFanout == nil {
		t.Fatal("reviewer node missing its ReviewFanout")
	}
}

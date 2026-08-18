package dag

import (
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

// TestNodeGateConfig_MultiReviewerPlanGetsSharedFanout pins #867: a plan with
// more than one code-reviewer node stamps every reviewer node's cfg with the
// SAME ReviewFanout instance (they must all fan into one accumulator) -
// non-reviewer nodes (explorer, implementer) never get one.
func TestNodeGateConfig_MultiReviewerPlanGetsSharedFanout(t *testing.T) {
	plan := Plan{ID: "plan-867", Nodes: []Node{
		{ID: "impl", AgentName: implementerAgent},
		{ID: "explore", AgentName: explorerAgent},
		{ID: "r1", AgentName: reviewerAgent},
		{ID: "r2", AgentName: reviewerAgent},
		{ID: "r3", AgentName: reviewerAgent},
	}}
	cfgFor := func(string) vetting.Config { return vetting.Config{} }

	implCfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1", "")
	if implCfg.ReviewFanout != nil {
		t.Error("implementer node must not get a ReviewFanout")
	}
	exploreCfg := nodeGateConfig(plan, plan.Nodes[1], nil, cfgFor, "chat1", "")
	if exploreCfg.ReviewFanout != nil {
		t.Error("explorer node must not get a ReviewFanout")
	}

	r1Cfg := nodeGateConfig(plan, plan.Nodes[2], nil, cfgFor, "chat1", "")
	r2Cfg := nodeGateConfig(plan, plan.Nodes[3], nil, cfgFor, "chat1", "")
	r3Cfg := nodeGateConfig(plan, plan.Nodes[4], nil, cfgFor, "chat1", "")
	if r1Cfg.ReviewFanout == nil || r2Cfg.ReviewFanout == nil || r3Cfg.ReviewFanout == nil {
		t.Fatal("every reviewer node in a multi-reviewer plan must get a ReviewFanout")
	}
	if r1Cfg.ReviewFanout != r2Cfg.ReviewFanout || r2Cfg.ReviewFanout != r3Cfg.ReviewFanout {
		t.Fatal("all reviewer nodes in the same plan must share ONE ReviewFanout instance")
	}
}

// TestNodeGateConfig_SingleReviewerPlanNoFanout pins the regression risk: a
// plan with exactly one code-reviewer node must NOT get a ReviewFanout -
// that node keeps delivering its own review exactly like before #867.
func TestNodeGateConfig_SingleReviewerPlanNoFanout(t *testing.T) {
	plan := Plan{ID: "plan-solo", Nodes: []Node{
		{ID: "impl", AgentName: implementerAgent},
		{ID: "r1", AgentName: reviewerAgent},
	}}
	cfgFor := func(string) vetting.Config { return vetting.Config{} }
	r1Cfg := nodeGateConfig(plan, plan.Nodes[1], nil, cfgFor, "chat1", "")
	if r1Cfg.ReviewFanout != nil {
		t.Fatal("a single-reviewer plan must not get a ReviewFanout")
	}
}

package dag

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

// Every reviewer node of a multi-reviewer plan shares one ReviewFanout; non-reviewer nodes
// never get one.
func TestNodeGateConfig_MultiReviewerPlanGetsSharedFanout(t *testing.T) {
	plan := Plan{ID: "plan-867", Nodes: []Node{
		{ID: "impl", AgentName: implementerAgent},
		{ID: "explore", AgentName: explorerAgent},
		{ID: "r1", AgentName: reviewerAgent},
		{ID: "r2", AgentName: reviewerAgent},
		{ID: "r3", AgentName: reviewerAgent},
	}}
	cfgFor := func(context.Context, string) vetting.Config { return vetting.Config{} }

	implCfg := nodeGateConfig(context.Background(), plan, plan.Nodes[0], nil, cfgFor, "chat1", "")
	if implCfg.ReviewFanout != nil {
		t.Error("implementer node must not get a ReviewFanout")
	}
	exploreCfg := nodeGateConfig(context.Background(), plan, plan.Nodes[1], nil, cfgFor, "chat1", "")
	if exploreCfg.ReviewFanout != nil {
		t.Error("explorer node must not get a ReviewFanout")
	}

	r1Cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[2], nil, cfgFor, "chat1", "")
	r2Cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[3], nil, cfgFor, "chat1", "")
	r3Cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[4], nil, cfgFor, "chat1", "")
	if r1Cfg.ReviewFanout == nil || r2Cfg.ReviewFanout == nil || r3Cfg.ReviewFanout == nil {
		t.Fatal("every reviewer node in a multi-reviewer plan must get a ReviewFanout")
	}
	if r1Cfg.ReviewFanout != r2Cfg.ReviewFanout || r2Cfg.ReviewFanout != r3Cfg.ReviewFanout {
		t.Fatal("all reviewer nodes in the same plan must share ONE ReviewFanout instance")
	}
}

// A single-reviewer plan gets no ReviewFanout; that node delivers its own review.
func TestNodeGateConfig_SingleReviewerPlanNoFanout(t *testing.T) {
	plan := Plan{ID: "plan-solo", Nodes: []Node{
		{ID: "impl", AgentName: implementerAgent},
		{ID: "r1", AgentName: reviewerAgent},
	}}
	cfgFor := func(context.Context, string) vetting.Config { return vetting.Config{} }
	r1Cfg := nodeGateConfig(context.Background(), plan, plan.Nodes[1], nil, cfgFor, "chat1", "")
	if r1Cfg.ReviewFanout != nil {
		t.Fatal("a single-reviewer plan must not get a ReviewFanout")
	}
}

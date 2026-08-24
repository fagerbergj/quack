package dag

import (
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/vetting"
)

// #1040: the review fan-in is a process-global keyed on plan ID alone, and it
// is cleaned up on exactly one happy path (deliverMergedReview -> forget). A
// run that reaches "delivered" but never gets to deliver - the node cancelled
// in between, the process restarted - leaves that state behind, and because a
// retry re-assembles the SAME plan ID it inherits it. Every later run of that
// plan then silently drops its review. Graph assembly must start a fresh
// fan-in per run.
func TestBuildGateNodes_RetryStartsAFreshReviewFanout(t *testing.T) {
	const planID = "plan-1040"
	stub := okStub{}
	mk := func(name string) adkagent.Agent {
		a, err := llmagent.New(llmagent.Config{Name: name, Model: stub, Description: name, Instruction: "ROLE"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return a
	}
	plan := Plan{ID: planID, UserMessage: "review it", Nodes: []Node{
		{ID: "rev1", AgentName: reviewerAgent},
		{ID: "rev2", AgentName: reviewerAgent},
	}}
	agents := map[string]adkagent.Agent{reviewerAgent: mk(reviewerAgent)}
	models := map[string]model.LLM{reviewerAgent: stub}

	// Run 1 completes the set (delivered=true) but is cancelled before
	// deliverMergedReview runs, so forget() never fires.
	f1 := vetting.GetReviewFanout(planID, 2)
	f1.Finish("rev1", vetting.StagedDelivery{Kind: "review", Event: "comment", Body: "a"}, true, false)
	if _, deliver := f1.Finish("rev2", vetting.StagedDelivery{Kind: "review", Event: "comment", Body: "b"}, true, false); !deliver {
		t.Fatal("run 1 should have reached the deliver signal")
	}

	// The retry: same plan ID, fresh graph assembly.
	if _, _, err := buildGateNodes(plan, agents, models, vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} },
		nil, nil, "chat-1", "app", nil, nil, nil); err != nil {
		t.Fatalf("assemble retry: %v", err)
	}

	f2 := vetting.GetReviewFanout(planID, 2)
	f2.Finish("rev1", vetting.StagedDelivery{Kind: "review", Event: "request_changes", Body: "retry a"}, true, false)
	_, deliver := f2.Finish("rev2", vetting.StagedDelivery{Kind: "review", Event: "request_changes", Body: "retry b"}, true, false)
	if !deliver {
		t.Fatal("retried run never delivers its review: assembly reused the previous run's fan-in")
	}
}

package vetting

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/decide/decidetest"
	"github.com/fagerbergj/quack/internal/ledger"
)

// evidenceJudge submits a verdict for "grounded", giving evidence only when its request offers the field,
// as a schema-following model would, and reports 42 output + 7 reasoning tokens for the turn.
type evidenceJudge struct{ offered *bool }

func (evidenceJudge) Name() string { return "evidence-judge" }

func (j evidenceJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	cfg, _ := json.Marshal(req.Config)
	*j.offered = strings.Contains(string(cfg), `"evidence":{`) && strings.Contains(stubAllText(req)+string(cfg), "`evidence`")
	crit := map[string]any{"score": 2}
	if *j.offered {
		crit["evidence"] = "read a.go:10, it matches the claim"
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		r := stubCall(submitVerdictTool, map[string]any{"score": 2, "feedback": "", "criteria": map[string]any{"grounded": crit}})
		r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{CandidatesTokenCount: 42, ThoughtsTokenCount: 7}
		yield(r, nil)
	}
}

func replayGroundedCfg() Config {
	return Config{Threshold: 0.5, Agent: "web-researcher", RubricSpecs: map[string]criterionSpec{"grounded": criteriaSpecs["grounded"]}}
}

// TestReplayRoundJudgeCriteriaOnArm: with judge.criteria enabled the replayed judge is asked for evidence,
// the point answers from it and the result carries both, plus the judge's cost.
func TestReplayRoundJudgeCriteriaOnArm(t *testing.T) {
	mem := decidetest.Capture(t)
	srv := newScoreSrv(t, nil, 0)
	var offered bool
	cfg := replayGroundedCfg()
	cfg.Decisions = decidetest.Decider(t, srv.URL, judgeCriteria.ID)
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-replay", Node: "n1", Agent: "web-researcher"})

	res, err := ReplayRound(ctx, cfg, NewJudgeFactory(evidenceJudge{&offered}, nil, nil), ReplayCase{NodeID: "n1", Task: "t", Answer: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if !offered {
		t.Error("the replayed judge was not offered the evidence clause and field")
	}
	if len(res.Criteria) != 1 {
		t.Fatalf("criteria = %+v, want grounded", res.Criteria)
	}
	c := res.Criteria[0]
	if c.Evidence != "read a.go:10, it matches the claim" || c.Point == nil || c.Point.Top != "2" || c.Point.Probabilities["2"] != 0.8 || c.Point.GradeWords || c.Point.Error != "" {
		t.Errorf("criterion = %+v point %+v, want the evidence and the point's answer 2 (p 0.8)", c, c.Point)
	}
	if !res.JudgeCriteria {
		t.Error("JudgeCriteria = false on a run that offered the evidence field")
	}
	if res.Judge == nil || res.Judge.OutputTokens != 42 || res.Judge.ThoughtTokens != 7 || res.Judge.Seconds <= 0 {
		t.Errorf("judge cost = %+v, want 42 output, 7 thought tokens and a wall time", res.Judge)
	}
	if got := decidetest.Records(t, mem, "chat-replay", 1); len(got) != 1 || got[0].Baseline != "2" || !strings.Contains(string(got[0].Meta), `"criterion":"grounded"`) {
		t.Errorf("records = %+v, want one judge.criteria record for grounded with baseline 2", got)
	}
}

// TestReplayRoundJudgeCriteriaOffArm: no point means the judge's request and the result are as before.
func TestReplayRoundJudgeCriteriaOffArm(t *testing.T) {
	srv := newScoreSrv(t, nil, 0)
	var offered bool
	cfg := replayGroundedCfg()
	cfg.Decisions = decidetest.Decider(t, srv.URL, answerAccept.ID) // another point on: judge.criteria stays off

	res, err := ReplayRound(context.Background(), cfg, NewJudgeFactory(evidenceJudge{&offered}, nil, nil), ReplayCase{NodeID: "n1", Task: "t", Answer: "a"})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Criteria[0]
	if res.JudgeCriteria || offered || c.Evidence != "" || c.Point != nil || len(srv.states) != 0 {
		t.Errorf("offered = %v evidence %q point %+v requests %d, want the judge unchanged and no ask", offered, c.Evidence, c.Point, len(srv.states))
	}
	if res.Judge == nil || res.Judge.OutputTokens != 42 {
		t.Errorf("judge cost = %+v, want it measured on the off arm too", res.Judge)
	}
}

// TestReplayRoundJudgeCriteriaNeedsThePromptBlock: the point enabled against a system/judge without the
// evidence block is not the on arm, and the result says so.
func TestReplayRoundJudgeCriteriaNeedsThePromptBlock(t *testing.T) {
	srv := newScoreSrv(t, nil, 0)
	var offered bool
	cfg := replayGroundedCfg()
	cfg.Decisions = decidetest.Decider(t, srv.URL, judgeCriteria.ID)
	cfg.judgePrompt = shippedJudgePrompt(t)
	delete(cfg.judgePrompt.blocks, "evidence")

	res, err := ReplayRound(context.Background(), cfg, NewJudgeFactory(evidenceJudge{&offered}, nil, nil), ReplayCase{NodeID: "n1", Task: "t", Answer: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if res.JudgeCriteria || offered {
		t.Errorf("JudgeCriteria = %v, evidence offered = %v; want neither without the prompt block", res.JudgeCriteria, offered)
	}
}

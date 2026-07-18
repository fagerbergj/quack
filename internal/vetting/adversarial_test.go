package vetting

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// stubSkepticModel is a scripted model.LLM that always calls
// submit_skeptic_verdict with the canned refuted/reason.
type stubSkepticModel struct {
	refuted bool
	reason  string
}

func (stubSkepticModel) Name() string { return "stub-skeptic" }

func (s stubSkepticModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(stubCall(submitSkepticVerdictTool, map[string]any{"refuted": s.refuted, "reason": s.reason}), nil)
	}
}

func testQuestionAnswer() (*genai.Content, string) {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "what does the code do"}}}, "it references cfg.RequireRetrieval"
}

// TestAdversarialVerify_MajorityRefuteKillsFinding pins #370's core rule: a
// STRICT MAJORITY of skeptics refuting a load-bearing, currently-passing
// criterion kills it (score → 0), which drags the weakest-link overall score
// down with it.
func TestAdversarialVerify_MajorityRefuteKillsFinding(t *testing.T) {
	q, a := testQuestionAnswer()
	cfg := Config{Threshold: 0.7, SkepticRounds: 3, Skeptic: NewSkepticFactory(stubSkepticModel{refuted: true, reason: "cfg.RequireRetrieval is not real"}, nil)}
	v := verdict{Score: 0.9, Criteria: map[string]criterionScore{
		"grounded": {Score: 0.9, Reason: "the answer cites specific config fields"},
	}}
	got := adversarialVerify(context.Background(), cfg, q, a, workerActivity{}, v, nil)
	c := got.Criteria["grounded"]
	if c.Score != 0 {
		t.Fatalf("grounded score = %v, want 0 (killed by majority refute)", c.Score)
	}
	if got.Score != 0 {
		t.Fatalf("overall score = %v, want 0 (weakest-link must reflect the killed finding)", got.Score)
	}
}

// TestAdversarialVerify_MajoritySurviveKeepsFinding proves the mirror case: a
// majority of skeptics failing to refute the finding leaves it untouched.
func TestAdversarialVerify_MajoritySurviveKeepsFinding(t *testing.T) {
	q, a := testQuestionAnswer()
	cfg := Config{Threshold: 0.7, SkepticRounds: 3, Skeptic: NewSkepticFactory(stubSkepticModel{refuted: false, reason: "checks out"}, nil)}
	v := verdict{Score: 0.9, Criteria: map[string]criterionScore{
		"grounded": {Score: 0.9, Reason: "the answer cites specific config fields"},
	}}
	got := adversarialVerify(context.Background(), cfg, q, a, workerActivity{}, v, nil)
	if got.Criteria["grounded"].Score != 0.9 {
		t.Fatalf("grounded score = %v, want unchanged 0.9 (majority survived)", got.Criteria["grounded"].Score)
	}
}

// TestAdversarialVerify_SkipsDeterministicAndFailingCriteria proves
// loadBearing's two exclusions: a foldDeterministic-owned criterion (its
// Reason is code-authored ground truth already, not a judge guess) and a
// criterion that's already failing (already caught; nothing to refute) are
// never sent to a skeptic. If either were, the always-refute skeptic below
// would zero them out.
func TestAdversarialVerify_SkipsDeterministicAndFailingCriteria(t *testing.T) {
	q, a := testQuestionAnswer()
	cfg := Config{Threshold: 0.7, SkepticRounds: 1, Skeptic: NewSkepticFactory(stubSkepticModel{refuted: true, reason: "refuted"}, nil)}
	v := verdict{Criteria: map[string]criterionScore{
		"checks_pass":   {Score: 1.0, Reason: "deterministic: 3 check(s) passed"},
		"cites_sources": {Score: 0.3, Reason: "few links"},
	}}
	got := adversarialVerify(context.Background(), cfg, q, a, workerActivity{}, v, nil)
	if got.Criteria["checks_pass"].Score != 1.0 {
		t.Fatalf("checks_pass must never be sent to a skeptic, got score %v", got.Criteria["checks_pass"].Score)
	}
	if got.Criteria["cites_sources"].Score != 0.3 {
		t.Fatalf("an already-failing criterion must never be sent to a skeptic, got score %v", got.Criteria["cites_sources"].Score)
	}
}

// TestAdversarialVerify_NoOpWhenDisabled proves the stage is a true no-op
// (default) when cfg.Skeptic is unset — the common case, since Skeptics
// defaults to 0.
func TestAdversarialVerify_NoOpWhenDisabled(t *testing.T) {
	q, a := testQuestionAnswer()
	v := verdict{Criteria: map[string]criterionScore{"grounded": {Score: 0.9, Reason: "cited"}}}
	got := adversarialVerify(context.Background(), Config{Threshold: 0.7}, q, a, workerActivity{}, v, nil)
	if got.Criteria["grounded"].Score != 0.9 {
		t.Fatalf("disabled adversarial verify must not touch criteria, got %v", got.Criteria["grounded"].Score)
	}
}

// alternatingSkepticModel refutes on its first call and survives on its
// second (and repeats), so N rounds against it produce an even split whenever
// N is even — the tie case.
type alternatingSkepticModel struct{ calls int }

func (*alternatingSkepticModel) Name() string { return "alternating-skeptic" }

func (m *alternatingSkepticModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		refuted := m.calls%2 == 0
		m.calls++
		yield(stubCall(submitSkepticVerdictTool, map[string]any{"refuted": refuted, "reason": "split"}), nil)
	}
}

// TestAdversarialVerify_TieFavoursThePrimaryJudge proves the documented
// tie-break: an even split (1 refute of 2) is NOT a strict majority, so the
// primary judge's PASS survives.
func TestAdversarialVerify_TieFavoursThePrimaryJudge(t *testing.T) {
	q, a := testQuestionAnswer()
	cfg := Config{Threshold: 0.7, SkepticRounds: 2, Skeptic: NewSkepticFactory(&alternatingSkepticModel{}, nil)}
	v := verdict{Criteria: map[string]criterionScore{"grounded": {Score: 0.9, Reason: "cited"}}}
	got := adversarialVerify(context.Background(), cfg, q, a, workerActivity{}, v, nil)
	if got.Criteria["grounded"].Score != 0.9 {
		t.Fatalf("a 1-1 split must not kill the finding, got score %v", got.Criteria["grounded"].Score)
	}
}

package vetting

import (
	"context"
	"iter"
	"sync/atomic"
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
// (default) when cfg.Skeptic is unset - the common case, since Skeptics
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
// N is even - the tie case.
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

// patternSkepticModel yields a scripted refuted/survives verdict per call, in
// order (repeating the last entry if called more times than scripted) - lets
// a test pin an exact N-of-M split rather than only the unanimous cases.
type patternSkepticModel struct {
	calls   int
	pattern []bool // true = refuted
}

func (*patternSkepticModel) Name() string { return "pattern-skeptic" }

func (m *patternSkepticModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		i := m.calls
		if i >= len(m.pattern) {
			i = len(m.pattern) - 1
		}
		refuted := m.pattern[i]
		m.calls++
		yield(stubCall(submitSkepticVerdictTool, map[string]any{"refuted": refuted, "reason": "scripted"}), nil)
	}
}

// garbledSkepticModel always calls submit_skeptic_verdict with an empty args
// map - the same truncated-tool-call shape as judge.go's garbledVerdictJudge
// - so schema validation (both fields are required) rejects it before the
// handler that populates sink ever runs.
type garbledSkepticModel struct{}

func (garbledSkepticModel) Name() string { return "garbled-skeptic" }

func (garbledSkepticModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(stubCall(submitSkepticVerdictTool, map[string]any{}), nil)
	}
}

// TestAdversarialVerify_GarbledSubmitDefaultsToRefuted proves #889's fix on
// the skeptic side: a garbled/schema-rejected submit_skeptic_verdict call
// must fall to the documented fail-closed default (refuted), never the zero
// value Refuted=false a naively "submitted" empty verdict would carry - which
// would be the OPPOSITE of fail-closed.
func TestAdversarialVerify_GarbledSubmitDefaultsToRefuted(t *testing.T) {
	q, a := testQuestionAnswer()
	cfg := Config{Threshold: 0.7, SkepticRounds: 1, Skeptic: NewSkepticFactory(garbledSkepticModel{}, nil)}
	v := verdict{Criteria: map[string]criterionScore{
		"grounded": {Score: 0.9, Reason: "the answer cites specific config fields"},
	}}
	got := adversarialVerify(context.Background(), cfg, q, a, workerActivity{}, v, nil)
	if got.Criteria["grounded"].Score != 0 {
		t.Fatalf("grounded score = %v, want 0 - a garbled skeptic call must fail closed (refuted), not silently survive", got.Criteria["grounded"].Score)
	}
}

// maxTokensRecordingSkeptic records the MaxOutputTokens the request actually
// carried before submitting a normal verdict, proving the shared judge/
// skeptic cap reaches the skeptic's own model request too (#889).
type maxTokensRecordingSkeptic struct{ got *int32 }

func (maxTokensRecordingSkeptic) Name() string { return "max-tokens-recording-skeptic" }

func (m maxTokensRecordingSkeptic) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if req.Config != nil {
			atomic.StoreInt32(m.got, req.Config.MaxOutputTokens)
		}
		yield(stubCall(submitSkepticVerdictTool, map[string]any{"refuted": false, "reason": "checks out"}), nil)
	}
}

// TestAdversarialVerify_RequestCarriesConfiguredMaxOutputTokens proves
// cfg.JudgeMaxOutputTokens reaches the skeptic's own model request.
func TestAdversarialVerify_RequestCarriesConfiguredMaxOutputTokens(t *testing.T) {
	q, a := testQuestionAnswer()
	got := int32(-1)
	cfg := Config{Threshold: 0.7, SkepticRounds: 1, JudgeMaxOutputTokens: 2048,
		Skeptic: NewSkepticFactory(maxTokensRecordingSkeptic{got: &got}, nil)}
	v := verdict{Criteria: map[string]criterionScore{"grounded": {Score: 0.9, Reason: "cited"}}}
	adversarialVerify(context.Background(), cfg, q, a, workerActivity{}, v, nil)
	if got != 2048 {
		t.Errorf("req.Config.MaxOutputTokens = %d, want 2048", got)
	}
}

// TestAdversarialVerify_TwoOfThreeMajority pins the exact majority rule the
// earlier always-refute (3/3) test could not: 2 refutes out of 3 is a STRICT
// majority and kills the finding, but 1 out of 3 is not and leaves it alone -
// distinguishing "majority" from "unanimity" (a buggy unanimous-refute rule
// would also pass the 3/3 test but fail these).
func TestAdversarialVerify_TwoOfThreeMajority(t *testing.T) {
	q, a := testQuestionAnswer()

	t.Run("2 of 3 refute kills it", func(t *testing.T) {
		cfg := Config{Threshold: 0.7, SkepticRounds: 3, Skeptic: NewSkepticFactory(&patternSkepticModel{pattern: []bool{true, true, false}}, nil)}
		v := verdict{Criteria: map[string]criterionScore{"grounded": {Score: 0.9, Reason: "cited"}}}
		got := adversarialVerify(context.Background(), cfg, q, a, workerActivity{}, v, nil)
		if got.Criteria["grounded"].Score != 0 {
			t.Fatalf("2/3 refuted must kill the finding, got score %v", got.Criteria["grounded"].Score)
		}
	})

	t.Run("1 of 3 refute survives", func(t *testing.T) {
		cfg := Config{Threshold: 0.7, SkepticRounds: 3, Skeptic: NewSkepticFactory(&patternSkepticModel{pattern: []bool{true, false, false}}, nil)}
		v := verdict{Criteria: map[string]criterionScore{"grounded": {Score: 0.9, Reason: "cited"}}}
		got := adversarialVerify(context.Background(), cfg, q, a, workerActivity{}, v, nil)
		if got.Criteria["grounded"].Score != 0.9 {
			t.Fatalf("1/3 refuted must not be a majority, got score %v (want unchanged 0.9)", got.Criteria["grounded"].Score)
		}
	})
}

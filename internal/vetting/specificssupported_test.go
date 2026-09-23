package vetting

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// seqLLM answers its calls in order, repeating the last answer; prompts records what each call saw.
type seqLLM struct {
	answers []string
	prompts *[]string
}

func (seqLLM) Name() string { return "seq-llm" }

func (m seqLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var b strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			b.WriteString(p.Text)
		}
	}
	*m.prompts = append(*m.prompts, b.String())
	text := m.answers[min(len(*m.prompts), len(m.answers))-1]
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}}, nil)
	}
}

const secondLookPage = "filler " + "about the survey and its method, " + "users rose 30% in 2024, not 25% as first reported, per the audited figures."

func secondLookCheck() UnitCheck {
	page := strings.Repeat("padding words before the passage. ", 40) + secondLookPage + strings.Repeat(" padding words after the passage.", 40)
	c := UnitCheck{Unit: Unit{Text: "Users rose 25% in 2024."}, Specific: Specific{Kind: "percent", Value: "25%", Norm: "25"}, Citation: "https://p/1", State: "located", page: page}
	c.Window, _ = LocateSpecific(page, c.Specific)
	return c
}

func TestVerifyChecks_SecondLookDecidesAnUnsupportedItem(t *testing.T) {
	first := `{"items":[{"n":1,"state":"unsupported","quote":"users rose 30% in 2024"}]}`
	cases := []struct {
		name, second, want, reason string
	}{
		{"confirmed", first, "unsupported", "confirmed"},
		{"overturned", `{"items":[{"n":1,"state":"supported","quote":"not 25% as first reported"}]}`, "supported", ""},
		{"unsettled", `{"items":[{"n":1,"state":"cannot_tell","quote":""}]}`, "cannot_tell", "did not confirm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var prompts []string
			got := Verifier{LLM: seqLLM{answers: []string{first, tc.second}, prompts: &prompts}}.VerifyChecks(context.Background(), []UnitCheck{secondLookCheck()})
			if len(prompts) != 2 {
				t.Fatalf("calls = %d, want a first read and one second look", len(prompts))
			}
			if len(prompts[1]) <= len(prompts[0]) {
				t.Errorf("second look evidence is not wider: %d <= %d chars", len(prompts[1]), len(prompts[0]))
			}
			if v := got[0].Verdict; v.State != tc.want || !strings.Contains(v.Reason, tc.reason) {
				t.Errorf("verdict = %+v, want state %q with reason containing %q", v, tc.want, tc.reason)
			}
		})
	}
}

func TestVerifyChecks_SupportedItemGetsNoSecondLook(t *testing.T) {
	var prompts []string
	ok := `{"items":[{"n":1,"state":"supported","quote":"not 25% as first reported"}]}`
	Verifier{LLM: seqLLM{answers: []string{ok}, prompts: &prompts}}.VerifyChecks(context.Background(), []UnitCheck{secondLookCheck()})
	if len(prompts) != 1 {
		t.Errorf("calls = %d, want 1", len(prompts))
	}
}

func TestSpecificsSupportedScore(t *testing.T) {
	with := func(states ...string) []UnitCheck {
		var out []UnitCheck
		for _, s := range states {
			c := secondLookCheck()
			c.Verdict = Verdict{State: s, Quote: "users rose 30% in 2024"}
			out = append(out, c)
		}
		return out
	}
	if _, ok := specificsSupportedScore(with("cannot_tell", "not_checked")); ok {
		t.Error("nothing supported or contradicted: the criterion must be absent, not a pass or a fail")
	}
	if c, ok := specificsSupportedScore(with("supported", "cannot_tell")); !ok || c.Score != 1 {
		t.Errorf("all read specifics supported: got ok=%v %+v", ok, c)
	}
	c, ok := specificsSupportedScore(with("supported", "unsupported"))
	if !ok || c.Score != 0 || len(c.Evidence) != 1 || !strings.Contains(c.Reason, `the page says "users rose 30% in 2024"`) {
		t.Errorf("one contradicted specific must fail with the page's words: ok=%v %+v", ok, c)
	}
}

func TestStartVerify(t *testing.T) {
	u := "https://example.test/survey"
	pages := fakeLoader{pageID(t, u): secondLookPage}
	answer := "Users rose 25% in 2024 ([survey](" + u + ")). Churn fell to 4.2% ([survey](" + u + "))."
	cfg := Config{RecordReader: pages, RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}}}

	var prompts []string
	cfg.JudgeModel = seqLLM{answers: []string{`{"items":[{"n":1,"state":"unsupported","quote":"users rose 30% in 2024"}]}`}, prompts: &prompts}
	c, ok := startVerify(context.Background(), cfg, answer, workerActivity{})()
	if !ok || c.Score != 0 || !strings.Contains(c.Reason, `"25%"`) {
		t.Errorf("contradicted figure on its cited page must fail the criterion: ok=%v %+v (prompts %d)", ok, c, len(prompts))
	}

	undeclared := cfg
	undeclared.RubricSpecs = nil
	prompts = nil
	if _, ok := startVerify(context.Background(), undeclared, answer, workerActivity{})(); ok || len(prompts) != 0 {
		t.Errorf("a rubric that does not declare specifics_supported must not run it: ok=%v calls=%d", ok, len(prompts))
	}
}

// TestStartVerify_RacesTheJudgePath runs the verify goroutine while the judge
// path reads the same act and det, as runJudge does; meaningful under -race.
func TestStartVerify_RacesTheJudgePath(t *testing.T) {
	u := "https://example.test/survey"
	var prompts []string
	cfg := Config{RecordReader: fakeLoader{pageID(t, u): secondLookPage}, Threshold: 0.6,
		RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel:  seqLLM{answers: []string{`{"items":[{"n":1,"state":"supported","quote":"not 25% as first reported"}]}`}, prompts: &prompts}}
	act := workerActivity{fetched: map[string]struct{}{u: {}}, seen: map[string]string{}, paths: map[string]bool{}, artifactsWritten: []string{"text:none"}}
	det := map[string]criterionScore{"cites_sources": {Score: 1, Deterministic: true}}

	wait := startVerify(context.Background(), cfg, "Users rose 25% in 2024 ([survey]("+u+")).", act)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the judge's own reads while the verify tier runs
		defer wg.Done()
		for range 50 {
			changedFilesSection(cfg, act)
			judgeKnownFailuresSection(det, cfg.Threshold)
		}
	}()
	wg.Wait()
	c, ok := wait()
	if !ok || c.Score != 1 {
		t.Fatalf("specifics_supported = %+v ok=%v, want a pass", c, ok)
	}
	det[specificsSupportedCriterion] = c
}

func TestReplayRound_SpecificsSupported(t *testing.T) {
	u := "https://example.test/survey"
	unsupported := `{"items":[{"n":1,"state":"unsupported","quote":"users rose 30% in 2024"}]}`
	run := func(specs map[string]criterionSpec) *ReplayCriterion {
		var prompts []string
		rc := ReplayCase{NodeID: "node-1", Task: "task", Answer: "Users rose 25% in 2024 ([survey](" + u + ")).",
			Pages: fakeLoader{pageID(t, u): secondLookPage}, Verifier: &Verifier{LLM: seqLLM{answers: []string{unsupported}, prompts: &prompts}}}
		res, err := ReplayRound(context.Background(), Config{Threshold: 0.5, RubricSpecs: specs}, nil, rc)
		if err != nil {
			t.Fatalf("ReplayRound: %v", err)
		}
		for i := range res.Criteria {
			if res.Criteria[i].Name == specificsSupportedCriterion {
				return &res.Criteria[i]
			}
		}
		return nil
	}
	got := run(map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}})
	if got == nil || got.Score != 0 || !got.Deterministic {
		t.Errorf("specifics_supported = %+v, want a deterministic 0 for a contradicted figure", got)
	}
	if got := run(nil); got != nil {
		t.Errorf("an undeclaring rubric got specifics_supported: %+v", got)
	}
}

// TestVerifyChecks_SnippetNeverContradicts: a live page's search snippet is often
// stale (prod: an ADP page's snippet showed an older draft window), so it can only back a figure.
func TestVerifyChecks_SnippetNeverContradicts(t *testing.T) {
	c := secondLookCheck()
	c.snippet = true
	var prompts []string
	got := Verifier{LLM: seqLLM{answers: []string{`{"items":[{"n":1,"state":"unsupported","quote":"users rose 30% in 2024"}]}`}, prompts: &prompts}}.VerifyChecks(context.Background(), []UnitCheck{c})
	if got[0].Verdict.State != "cannot_tell" || len(prompts) != 1 {
		t.Errorf("snippet contradiction = %+v after %d calls, want cannot_tell with no second look", got[0].Verdict, len(prompts))
	}
	ok := `{"items":[{"n":1,"state":"supported","quote":"not 25% as first reported"}]}`
	prompts = nil
	if got := (Verifier{LLM: seqLLM{answers: []string{ok}, prompts: &prompts}}).VerifyChecks(context.Background(), []UnitCheck{c}); got[0].Verdict.State != "supported" {
		t.Errorf("a snippet that states the figure must back it: %+v", got[0].Verdict)
	}
}

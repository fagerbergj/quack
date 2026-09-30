package vetting

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// seqLLM answers its calls in order, repeating the last answer; prompts records what each call saw.
type seqLLM struct {
	answers []string
	prompts *[]string
}

var seqLLMMu sync.Mutex // the verifier calls pages concurrently

func (seqLLM) Name() string { return "seq-llm" }

func (m seqLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var b strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			b.WriteString(p.Text)
		}
	}
	seqLLMMu.Lock()
	*m.prompts = append(*m.prompts, b.String())
	text := m.answers[min(len(*m.prompts), len(m.answers))-1]
	seqLLMMu.Unlock()
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

func TestRunVerify(t *testing.T) {
	u := "https://example.test/survey"
	pages := fakeLoader{pageID(t, u): secondLookPage}
	answer := "Users rose 25% in 2024 ([survey](" + u + ")). Churn fell to 4.2% ([survey](" + u + "))."
	cfg := Config{RecordReader: pages, RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}}}
	act := workerActivity{fetched: map[string]struct{}{u: {}}}

	var prompts []string
	cfg.JudgeModel = seqLLM{answers: []string{`{"items":[{"n":1,"state":"unsupported","quote":"users rose 30% in 2024"}]}`}, prompts: &prompts}
	c, ok := specificsSupportedScore(runVerify(context.Background(), cfg, answer, act, nil))
	if !ok || c.Score != 0 || !strings.Contains(c.Reason, `"25%"`) {
		t.Errorf("contradicted figure on its cited page must fail the criterion: ok=%v %+v (prompts %d)", ok, c, len(prompts))
	}

	undeclared := cfg
	undeclared.RubricSpecs = nil
	prompts = nil
	if checks := runVerify(context.Background(), undeclared, answer, act, nil); checks != nil || len(prompts) != 0 {
		t.Errorf("a rubric that does not declare specifics_supported must not run it: checks=%v calls=%d", checks, len(prompts))
	}
}

// TestRunVerify_SiblingPageNeverBacksACitation: the page store is chat-wide, so a page
// only a sibling fetched is in it; this node never fetched it, so nothing is read from it.
func TestRunVerify_SiblingPageNeverBacksACitation(t *testing.T) {
	u := "https://example.test/survey"
	var prompts []string
	cfg := Config{RecordReader: fakeLoader{pageID(t, u): secondLookPage},
		RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel:  seqLLM{answers: []string{`{"items":[{"n":1,"state":"supported","quote":"not 25% as first reported"}]}`}, prompts: &prompts}}
	checks := runVerify(context.Background(), cfg, "Users rose 25% in 2024 ([survey]("+u+")).", workerActivity{}, nil)
	if len(checks) == 0 || checks[0].State != "no_stored_text" || len(prompts) != 0 {
		t.Fatalf("checks = %+v after %d verifier calls, want no_stored_text and no call", checks, len(prompts))
	}
	read := workerActivity{sourceReads: []string{pageID(t, u)}}
	if checks := runVerify(context.Background(), cfg, "Users rose 25% in 2024 ([survey]("+u+")).", read, nil); checks[0].State != "located" {
		t.Errorf("a page the node read itself must back its citation: %+v", checks[0])
	}
}

// TestRunVerify_MemoSkipsUnchangedClaims: a later round re-reads only the claims whose
// page, detail or evidence window changed; the rest reuse the earlier verdict.
func TestRunVerify_MemoSkipsUnchangedClaims(t *testing.T) {
	u := "https://example.test/survey"
	var prompts []string
	cfg := Config{RecordReader: fakeLoader{pageID(t, u): secondLookPage},
		RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel:  seqLLM{answers: []string{`{"items":[{"n":1,"state":"supported","quote":"users rose 30% in 2024"},{"n":2,"state":"supported","quote":"users rose 30% in 2024"}]}`}, prompts: &prompts}}
	act := workerActivity{fetched: map[string]struct{}{u: {}}}
	memo := map[string]Verdict{}
	answer := "Users rose 30% in 2024 ([survey](" + u + "))."
	for round := 1; round <= 2; round++ {
		checks := runVerify(context.Background(), cfg, answer, act, memo)
		if len(checks) != 2 || checks[0].Verdict.State != "supported" || checks[1].Verdict.State != "supported" {
			t.Fatalf("round %d: %+v, want the figure supported", round, checks)
		}
	}
	if len(prompts) != 1 {
		t.Errorf("verifier called %d times over two rounds of an unchanged claim, want 1", len(prompts))
	}
	runVerify(context.Background(), cfg, "Users rose 25% in 2024 ([survey]("+u+")).", act, memo)
	if len(prompts) != 2 {
		t.Errorf("a changed detail must be read again: %d calls, want 2", len(prompts))
	}
}

// TestRunVerify_MemoKeysOnClaimText: the same figure on the same page in a reworded claim is
// read again - a corrected claim loses its stale unsupported, a wrongly reworded one its supported.
func TestRunVerify_MemoKeysOnClaimText(t *testing.T) {
	u := "https://example.test/survey"
	item := func(state string) string {
		return `{"n":1,"state":"` + state + `","quote":"users rose 30% in 2024"},{"n":2,"state":"` + state + `","quote":"users rose 30% in 2024"}`
	}
	sup, uns := `{"items":[`+item("supported")+`]}`, `{"items":[`+item("unsupported")+`]}`
	var prompts []string
	cfg := Config{RecordReader: fakeLoader{pageID(t, u): secondLookPage},
		RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel:  seqLLM{answers: []string{sup, uns, uns, sup}, prompts: &prompts}}
	act := workerActivity{fetched: map[string]struct{}{u: {}}}
	memo := map[string]Verdict{}
	for _, step := range []struct {
		claim, want string
		calls       int
	}{
		{"Desktop users rose 30% in 2024", "supported", 1},
		{"Mobile users rose 30% in 2024", "unsupported", 3}, // a reworded claim, confirmed on a second look
		{"Desktop and mobile users rose 30% in 2024", "supported", 4},
		{"Desktop users rose 30% in 2024", "supported", 4}, // unchanged: memo hit
	} {
		checks := runVerify(context.Background(), cfg, step.claim+" ([survey]("+u+")).", act, memo)
		if checks[0].Verdict.State != step.want || len(prompts) != step.calls {
			t.Errorf("%q: %s after %d verifier calls, want %s after %d", step.claim, checks[0].Verdict.State, len(prompts), step.want, step.calls)
		}
	}
}

// TestVerifyChecks_FailedSecondLookNotMemoized: a second look that got no answer leaves a
// cannot_tell for this round only; the next round reads the claim again.
func TestVerifyChecks_FailedSecondLookNotMemoized(t *testing.T) {
	var prompts []string
	memo := map[string]Verdict{}
	v := Verifier{LLM: seqLLM{answers: []string{`{"items":[{"n":1,"state":"unsupported","quote":"users rose 30% in 2024"}]}`, `not json`}, prompts: &prompts}, Memo: memo}
	got := v.VerifyChecks(context.Background(), []UnitCheck{secondLookCheck()})
	if got[0].Verdict.State != "cannot_tell" || len(memo) != 0 {
		t.Errorf("verdict %+v, memo %v: want cannot_tell and nothing remembered", got[0].Verdict, memo)
	}
}

// TestJudgeEvidenceSection: only an unconfirmed specific reaches the judge, with its verifier
// result and a bounded excerpt; matched ones are only counted, uncited ones not listed.
func TestJudgeEvidenceSection(t *testing.T) {
	c := secondLookCheck()
	c.Window = strings.Repeat("x", 2000) + "users rose 30% in 2024" + strings.Repeat("y", 2000)
	c.Verdict = Verdict{State: "unsupported", Quote: "users rose 30% in 2024"}
	uncited := UnitCheck{Unit: Unit{Text: "Churn was 4%."}, Specific: Specific{Value: "4%"}, State: "uncited"}
	matched := secondLookCheck()
	matched.Specific, matched.Unit.Text = Specific{Kind: "number", Value: "2024", Norm: "2024"}, "Revenue doubled in 2024."
	matched.Verdict = Verdict{State: "supported", Quote: "in 2024"}
	got := judgeEvidenceSection([]UnitCheck{c, uncited, matched})
	for _, want := range []string{"CITED EVIDENCE", "1 matched", `"25%"`, "https://p/1", `unsupported - page says "users rose 30% in 2024"`, "users rose 30% in 2024y"} {
		if !strings.Contains(got, want) {
			t.Errorf("section missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Churn") || strings.Contains(got, "Revenue") || len(got) > len(judgeEvidenceHeader)+judgeExcerptChars+400 {
		t.Errorf("section lists an uncited or matched specific, or an unbounded excerpt (%d chars):\n%s", len(got), got)
	}
	if judgeEvidenceSection([]UnitCheck{uncited, matched}) != "" {
		t.Error("every cited specific matched: the section must be absent")
	}
	unread := UnitCheck{Unit: Unit{Text: "Churn was 4%.", Citations: []string{"https://q/2"}}, Specific: Specific{Value: "4%"}, State: "no_stored_text"}
	if got := evidenceEntry(unread); strings.Contains(got, `("")`) || strings.Contains(got, "()") || !strings.Contains(got, "(https://q/2)") {
		t.Errorf("entry for an unread page = %q, want the claim's own citation named", got)
	}
}

func TestReplayRound_SpecificsSupported(t *testing.T) {
	u := "https://example.test/survey"
	unsupported := `{"items":[{"n":1,"state":"unsupported","quote":"users rose 30% in 2024"}]}`
	run := func(specs map[string]criterionSpec) *ReplayCriterion {
		var prompts []string
		rc := ReplayCase{NodeID: "node-1", Task: "task", Answer: "Users rose 25% in 2024 ([survey](" + u + ")).", WorkerTurns: []RawTurn{fetchTurn(u)},
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

// gateLLM announces each verifier call on started, then holds it until gate closes.
type gateLLM struct {
	now, peak *atomic.Int32
	started   chan struct{}
	gate      chan struct{}
}

func (gateLLM) Name() string { return "gate-llm" }

func (m gateLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := m.now.Add(1)
		for p := m.peak.Load(); n > p && !m.peak.CompareAndSwap(p, n); p = m.peak.Load() {
		}
		m.started <- struct{}{}
		<-m.gate
		m.now.Add(-1)
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: `{"items":[]}`}}}}, nil)
	}
}

// TestVerifyChecks_ParallelOnlyOnGrantedSessions: batches run on the held session plus each
// session Admission grants now - all four when free, two on one grant, one when none is free.
func TestVerifyChecks_ParallelOnlyOnGrantedSessions(t *testing.T) {
	var checks []UnitCheck
	for i := range 10 {
		c := secondLookCheck()
		c.Citation = fmt.Sprintf("https://p/%d", i)
		checks = append(checks, c)
	}
	grantN := func(n int, outstanding *atomic.Int32) func() (func(), bool) {
		return func() (func(), bool) {
			if n == 0 {
				return nil, false
			}
			n--
			outstanding.Add(1)
			return func() { outstanding.Add(-1) }, true
		}
	}
	for _, tc := range []struct {
		name   string
		grants int
		unmet  bool // no admission ledger at all
		want   int32
	}{{"no ledger", 0, true, verifyConcurrency}, {"one free session", 1, false, 2}, {"none free", 0, false, 1}} {
		var now, peak, outstanding atomic.Int32
		llm := gateLLM{now: &now, peak: &peak, started: make(chan struct{}, len(checks)), gate: make(chan struct{})}
		v := Verifier{LLM: llm, TryAdmit: grantN(tc.grants, &outstanding)}
		if tc.unmet {
			v.TryAdmit = nil
		}
		done := make(chan struct{})
		go func() { v.VerifyChecks(context.Background(), slices.Clone(checks)); close(done) }()
		for range tc.want { // wait for the expected parallel calls before letting any finish
			select {
			case <-llm.started:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s: only some of %d parallel verifier calls started", tc.name, tc.want)
			}
		}
		close(llm.gate)
		<-done
		if peak.Load() != tc.want || outstanding.Load() != 0 {
			t.Errorf("%s: peak %d concurrent calls, %d sessions unreleased; want %d and 0", tc.name, peak.Load(), outstanding.Load(), tc.want)
		}
	}
}

// TestVerifierWorkers: one worker per session held or granted now, bounded by the batches and verifyConcurrency.
func TestVerifierWorkers(t *testing.T) {
	grants := func(n int) func() (func(), bool) {
		return func() (func(), bool) {
			if n == 0 {
				return nil, false
			}
			n--
			return func() {}, true
		}
	}
	for _, tc := range []struct {
		name    string
		v       Verifier
		batches int
		want    int
	}{
		{"no ledger", Verifier{}, 10, verifyConcurrency},
		{"no ledger, one batch", Verifier{}, 1, 1},
		{"none free", Verifier{TryAdmit: grants(0)}, 10, 1},
		{"one free", Verifier{TryAdmit: grants(1)}, 10, 2},
		{"all free", Verifier{TryAdmit: grants(99)}, 10, verifyConcurrency},
		{"two batches", Verifier{TryAdmit: grants(99)}, 2, 2},
	} {
		if got, release := tc.v.workers(tc.batches); got != tc.want {
			t.Errorf("%s: %d workers, want %d", tc.name, got, tc.want)
		} else {
			release()
		}
	}
}

// TestVerifyPrompt_StablePagePrefix: the page and its windows lead every verifier prompt over that
// page byte for byte, whatever claims follow or their order, so a re-read reuses the cached prefix.
func TestVerifyPrompt_StablePagePrefix(t *testing.T) {
	u := "https://example.test/stats"
	page := strings.Repeat("filler words. ", 80) + "users rose 30% in 2024." + strings.Repeat(" more filler.", 80) + " churn fell to 4.2% last year."
	var prompts []string
	cfg := Config{RecordReader: fakeLoader{pageID(t, u): page},
		RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel: seqLLM{answers: []string{`{"items":[{"n":1,"state":"supported","quote":"users rose 30% in 2024"},` +
			`{"n":2,"state":"supported","quote":"users rose 30% in 2024"},{"n":3,"state":"supported","quote":"churn fell to 4.2% last year"}]}`}, prompts: &prompts}}
	act := workerActivity{fetched: map[string]struct{}{u: {}}}
	memo := map[string]Verdict{}
	churn := "Churn fell to 4.2% ([s](" + u + "))."
	for _, answer := range []string{ // round 2 rewords the first claim and moves it last
		"Users rose 30% in 2024 ([s](" + u + ")). " + churn,
		churn + " Active users rose 30% in 2024 ([s](" + u + ")).",
	} {
		runVerify(context.Background(), cfg, answer, act, memo)
	}
	if len(prompts) != 2 {
		t.Fatalf("verifier calls = %d, want one per round", len(prompts))
	}
	p1, _, ok1 := strings.Cut(prompts[0], "Items:")
	p2, items2, ok2 := strings.Cut(prompts[1], "Items:")
	if strings.Contains(items2, "Churn") {
		t.Fatalf("round 2 re-asked the unchanged claim: %s", items2)
	}
	if !ok1 || !ok2 || p1 != p2 || !strings.Contains(p1, "<Window 2>") || strings.Contains(p1, "Users rose") {
		t.Errorf("prompt prefixes differ or carry a claim:\n%q\n%q", p1, p2)
	}
}

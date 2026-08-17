package vetting

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRecordSearchResults(t *testing.T) {
	seen := map[string]string{}
	resp := map[string]any{"results": []any{
		map[string]any{"url": "https://a.com/x", "snippet": "snippet a", "title": "A"},
		map[string]any{"url": "https://b.com/y", "snippet": "snippet b"},
		map[string]any{"title": "no url here"}, // skipped: no url
	}}
	recordSearchResults(seen, resp)
	if seen["https://a.com/x"] != "snippet a" {
		t.Errorf("a snippet = %q, want %q", seen["https://a.com/x"], "snippet a")
	}
	if seen["https://b.com/y"] != "snippet b" {
		t.Errorf("b snippet = %q", seen["https://b.com/y"])
	}
	if len(seen) != 2 {
		t.Errorf("seen has %d entries, want 2 (url-less result skipped)", len(seen))
	}
}

func TestCitationScoreLayers(t *testing.T) {
	// One cited URL per layer: exact-fetched(1.0), exact-searched(0.75),
	// same-host-fetched(0.5), same-host-searched(0.25), unbacked(0.0).
	answer := strings.Join([]string{
		"[a](https://ex.com/fetched-page)",   // exact fetched
		"[b](https://srch.com/exact-result)", // exact searched
		"[c](https://ex.com/other)",          // same host as a fetched page
		"[d](https://srch.com/other)",        // same host as a search result
		"[e](https://nowhere.com/made-up)",   // never seen
	}, " ")
	act := workerActivity{
		fetched: map[string]struct{}{"https://ex.com/fetched-page": {}},
		seen:    map[string]string{"https://srch.com/exact-result": "snip"},
	}

	score, details, ok := citationScore(answer, act)
	if !ok {
		t.Fatal("citationScore ok=false, want true (answer cites URLs)")
	}
	want := map[string]float64{
		"https://ex.com/fetched-page":   1.0,
		"https://srch.com/exact-result": 0.75,
		"https://ex.com/other":          0.5,
		"https://srch.com/other":        0.25,
		"https://nowhere.com/made-up":   0.0,
	}
	got := map[string]float64{}
	for _, d := range details {
		got[d.url] = d.score
	}
	for u, w := range want {
		if got[u] != w {
			t.Errorf("citation %s scored %.2f, want %.2f", u, got[u], w)
		}
	}
	wantMean := (1.0 + 0.75 + 0.5 + 0.25 + 0.0) / 5
	if score != wantMean {
		t.Errorf("mean score = %.3f, want %.3f", score, wantMean)
	}
}

// TestCiteReasonNamesUnretrievedLinks is issue #789 test cases 1 and 2: an
// answer citing one fetched and two never-retrieved URLs must name both
// unretrieved ones in the reason, and must NOT name the fetched (passing) one.
func TestCiteReasonNamesUnretrievedLinks(t *testing.T) {
	answer := strings.Join([]string{
		"[a](https://ex.com/fetched)",
		"[b](https://nowhere.com/never-retrieved-1)",
		"[c](https://elsewhere.com/never-retrieved-2)",
	}, " ")
	act := workerActivity{fetched: map[string]struct{}{"https://ex.com/fetched": {}}}
	score, details, ok := citationScore(answer, act)
	if !ok {
		t.Fatal("citationScore ok=false, want true")
	}
	reason := citeReason(score, details)
	for _, want := range []string{"https://nowhere.com/never-retrieved-1", "https://elsewhere.com/never-retrieved-2"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason missing unretrieved link %q: %q", want, reason)
		}
	}
	if strings.Contains(reason, "https://ex.com/fetched") {
		t.Errorf("reason names the fetched (passing) link - a passing item is noise: %q", reason)
	}
}

// TestCiteReasonScoreUnchanged is issue #789 test case 3: adding detail to
// the reason must never move citationScore's own score.
func TestCiteReasonScoreUnchanged(t *testing.T) {
	answer := strings.Join([]string{
		"[a](https://ex.com/fetched)",
		"[b](https://nowhere.com/never-retrieved-1)",
		"[c](https://elsewhere.com/never-retrieved-2)",
	}, " ")
	act := workerActivity{fetched: map[string]struct{}{"https://ex.com/fetched": {}}}
	score, details, ok := citationScore(answer, act)
	if !ok {
		t.Fatal("citationScore ok=false, want true")
	}
	_ = citeReason(score, details) // building the reason must be read-only over score
	if wantScore := (1.0 + 0.0 + 0.0) / 3; score != wantScore {
		t.Errorf("score = %.3f, want %.3f - citeReason must not perturb citationScore's contract", score, wantScore)
	}
	det, _ := computeDeterministicCriteria(t.Context(), answer, act, Config{})
	if det["cites_sources"].Score != score {
		t.Errorf("cites_sources score = %.3f, want %.3f (identical to citationScore's own output)", det["cites_sources"].Score, score)
	}
}

// TestCiteReasonBoundsLongList is issue #789 test case 4: many unbacked URLs
// must not each get a line - the reason bounds the list and states the elided count.
func TestCiteReasonBoundsLongList(t *testing.T) {
	var links []string
	for i := 0; i < 40; i++ {
		links = append(links, fmt.Sprintf("[l%d](https://unbacked-%d.example.com/x)", i, i))
	}
	answer := strings.Join(links, " ")
	// citationScore requires some retrieval activity to engage at all - a
	// fetch unrelated to any of the 40 cited links keeps all 40 unbacked.
	act := workerActivity{fetched: map[string]struct{}{"https://other.example.com/unrelated": {}}}
	score, details, ok := citationScore(answer, act)
	if !ok {
		t.Fatal("citationScore ok=false, want true")
	}
	if len(details) != 40 {
		t.Fatalf("details has %d entries, want 40", len(details))
	}
	reason := citeReason(score, details)
	if got := strings.Count(reason, "unbacked-"); got != maxCiteReasonLinks {
		t.Errorf("reason names %d links, want bounded to %d", got, maxCiteReasonLinks)
	}
	wantElided := fmt.Sprintf("%d more elided", 40-maxCiteReasonLinks)
	if !strings.Contains(reason, wantElided) {
		t.Errorf("reason missing elided count %q: %q", wantElided, reason)
	}
}

// TestCiteReasonSortsWorstFirstBeforeTruncating: answer order lists the
// least-bad link first and the worst-scored link last - the reported bug
// truncated to the first 10 in answer order and elided the worst offenders.
// The fix must sort ascending by score before capping.
func TestCiteReasonSortsWorstFirstBeforeTruncating(t *testing.T) {
	var details []citationDetail
	for i := 0; i < 15; i++ {
		details = append(details, citationDetail{
			url:   fmt.Sprintf("https://link%d.example.com/p", i),
			score: 0.75 - float64(i)*0.05, // i=0 -> 0.75 (least-bad) .. i=14 -> 0.05 (worst)
		})
	}
	reason := citeReason(0.4, details)

	// The 10 lowest-scored links (i=5..14) must survive the cap.
	for i := 5; i < 15; i++ {
		want := fmt.Sprintf("https://link%d.example.com/p (%.2f)", i, 0.75-float64(i)*0.05)
		if !strings.Contains(reason, want) {
			t.Errorf("reason missing worst-scored link %q: %q", want, reason)
		}
	}
	// The 5 least-bad links (i=0..4) must be elided, not named.
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("link%d.example.com", i)
		if strings.Contains(reason, want) {
			t.Errorf("reason names least-bad link %q that should have been elided: %q", want, reason)
		}
	}
	if !strings.Contains(reason, "5 more elided") {
		t.Errorf("reason missing elided count %q", reason)
	}
}

// TestCiteReasonIncludesLegend is the #789 follow-up: the reason must carry
// the score-tier legend and remedy exactly once, self-contained for a
// revising worker with no other context - and only when there's something unbacked.
func TestCiteReasonIncludesLegend(t *testing.T) {
	fullyBacked := []citationDetail{{url: "https://ex.com/a", score: 1.0}}
	if reason := citeReason(1.0, fullyBacked); strings.Contains(reason, citeReasonLegend) {
		t.Errorf("fully-backed reason should not carry the legend: %q", reason)
	}

	unbacked := []citationDetail{
		{url: "https://a.example.com/p", score: 0.5},
		{url: "https://b.example.com/p", score: 0.25},
	}
	reason := citeReason(0.375, unbacked)
	if !strings.Contains(reason, citeReasonLegend) {
		t.Errorf("reason missing legend/remedy text: %q", reason)
	}
	if got := strings.Count(reason, "backing tiers:"); got != 1 {
		t.Errorf("legend must appear exactly once regardless of unbacked link count, got %d: %q", got, reason)
	}
}

func TestCitationScoreNormalizesAnchorsAndSlashes(t *testing.T) {
	// Cited URL differs from the fetched one only by a trailing slash and a #anchor
	// and host casing - should still score a 1.0 exact-fetch match.
	answer := "See [x](https://Ex.com/Page/#section)."
	act := workerActivity{fetched: map[string]struct{}{"https://ex.com/Page": {}}}
	score, _, ok := citationScore(answer, act)
	if !ok || score != 1.0 {
		t.Errorf("score=%.2f ok=%v, want 1.0 true (anchor/slash/case normalized)", score, ok)
	}
}

// TestCitationScoreSkipsAnchorsAndNonWebSchemes: in-document anchors, mailto:
// targets, and local file paths are not web-gradeable - they must not enter
// the mean at all (local citations are no longer deterministically checked).
func TestCitationScoreSkipsAnchorsAndNonWebSchemes(t *testing.T) {
	act := workerActivity{fetched: map[string]struct{}{"https://ex.com/a": {}}}
	answer := "[sec](#usage) [mail](mailto:a@b.com) [local](repo/file.go) [real](https://ex.com/a)"
	score, details, ok := citationScore(answer, act)
	if !ok || len(details) != 1 || score != 1.0 {
		t.Errorf("score=%.2f details=%+v ok=%v, want 1.0 with exactly the web link graded", score, details, ok)
	}
}

func TestCitationScoreNoCitations(t *testing.T) {
	act := workerActivity{fetched: map[string]struct{}{"https://ex.com/a": {}}}
	_, _, ok := citationScore("A plain answer with no links.", act)
	if ok {
		t.Error("citationScore ok=true for an answer with no URLs, want false")
	}
}

// TestCitationScoreSkippedWithoutRetrieval is the regression that matters
// most for this check (removal of local-citation scoring, see judge.go):
// a code-only node that never fetched or searched the web must cleanly
// abstain (ok=false, "nothing to grade") rather than scoring 0 and forcing a
// revision round - even though it cited local files inline.
func TestCitationScoreSkippedWithoutRetrieval(t *testing.T) {
	answer := "Per quack@internal/foo.go:1-5, [see also](internal/bar.go)."
	act := workerActivity{clonedRepos: []string{"https://github.com/org/repo"}}
	if _, _, ok := citationScore(answer, act); ok {
		t.Error("citationScore ok=true for a code-only node with no web activity, want false (nothing to grade)")
	}
}

func TestLengthScore(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
	}{
		{"", 0.0},
		{"   \n\t ", 0.0},
		{"a", 1.0},
		{"a full enough answer", 1.0},
	} {
		if got := lengthScore(c.in); got != c.want {
			t.Errorf("lengthScore(%q) = %.1f, want %.1f", c.in, got, c.want)
		}
	}
}

// TestSufficientLengthReasonStatesActualAndRequired is issue #789 test case
// 5: an empty answer's sufficient_length reason states both the actual
// length and the length that would pass, not just a bare char count.
func TestSufficientLengthReasonStatesActualAndRequired(t *testing.T) {
	det, _ := computeDeterministicCriteria(t.Context(), "   ", workerActivity{}, Config{})
	c, ok := det["sufficient_length"]
	if !ok {
		t.Fatal("sufficient_length missing for an empty answer")
	}
	if !strings.Contains(c.Reason, "0 chars") {
		t.Errorf("reason missing actual length: %q", c.Reason)
	}
	if !strings.Contains(c.Reason, fmt.Sprintf("%d", minAnswerChars)) {
		t.Errorf("reason missing the passing threshold (%d): %q", minAnswerChars, c.Reason)
	}
	if c.Score != 0.0 {
		t.Errorf("score = %.1f, want 0.0 (lengthScore unchanged)", c.Score)
	}
}

func TestNormalizeURL(t *testing.T) {
	for _, c := range []struct {
		in       string
		wantNorm string
		wantHost string
	}{
		{"https://Ex.com/Page/#section", "https://ex.com/Page", "ex.com"},
		{"http://A.com/", "http://a.com/", "a.com"},          // root path kept
		{"https://b.com/x/y/", "https://b.com/x/y", "b.com"}, // trailing slash trimmed
		{"not a url", "not a url", ""},                       // parse fallback
	} {
		gotNorm, gotHost := normalizeURL(c.in)
		if gotNorm != c.wantNorm || gotHost != c.wantHost {
			t.Errorf("normalizeURL(%q) = (%q,%q), want (%q,%q)", c.in, gotNorm, gotHost, c.wantNorm, c.wantHost)
		}
	}
}

func TestParseVerdictToleratesFencedJSON(t *testing.T) {
	// Fenced block is stripped, and the 0–10 score is normalized to 0–1.
	v, err := parseVerdict("```json\n{\"score\": 8, \"passed\": true, \"feedback\": \"x\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if v.Score != 0.8 {
		t.Errorf("score = %v, want 0.8 (8/10 normalized)", v.Score)
	}
}

func TestParseVerdictMisplacedTopLevel(t *testing.T) {
	// Reproduces the exact failure seen in prod: the model nested score/passed/feedback
	// inside criteria and omitted the outer closing brace.
	malformed := `{"criteria":{"grounded":{"reason":"good","score":0.9},"no_fabrication":{"reason":"ok","score":1.0},"answers_question":{"reason":"yes","score":1.0},"internally_consistent":{"reason":"fine","score":0.9},"cites_sources":{"reason":"none","score":0.0},"score":0.76,"passed":true,"feedback":"add citations"}`

	v, err := parseVerdict(malformed)
	if err != nil {
		t.Fatalf("parseVerdict(misplaced): %v", err)
	}
	// cites_sources=0 is the lowest criterion → overall score is that minimum (0.0)
	if v.Score != 0 {
		t.Errorf("score = %.2f, want 0 (lowest criterion: cites_sources=0)", v.Score)
	}
	// Feedback recovered from misplaced entry
	if v.Feedback != "add citations" {
		t.Errorf("feedback = %q, want recovered from criteria", v.Feedback)
	}
	// The 5 real criteria should be present; score/passed/feedback should not
	for _, want := range []string{"grounded", "no_fabrication", "answers_question", "internally_consistent", "cites_sources"} {
		if _, ok := v.Criteria[want]; !ok {
			t.Errorf("criteria missing %q", want)
		}
	}
	for _, bad := range []string{"score", "passed", "feedback"} {
		if _, ok := v.Criteria[bad]; ok {
			t.Errorf("criteria should not contain %q", bad)
		}
	}
}

func TestParseVerdictDuplicatedBlob(t *testing.T) {
	// Model emitted the JSON object twice (back-to-back); only the first should be parsed.
	blob := `{"score":0.8,"passed":true,"feedback":"ok"}`
	v, err := parseVerdict(blob + blob)
	if err != nil {
		t.Fatalf("parseVerdict(duplicated): %v", err)
	}
	if !v.Passed || v.Score != 0.8 {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseVerdictLowestCriterion(t *testing.T) {
	// Well-formed G-Eval verdict; the overall score is the lowest criterion, so
	// cites_sources=0 sinks it to 0.0 regardless of the model's holistic 0.96.
	input := `{"criteria":{"grounded":{"score":0.9},"no_fabrication":{"score":1.0},"answers_question":{"score":1.0},"internally_consistent":{"score":0.9},"cites_sources":{"score":0.0}},"score":0.96,"passed":true,"feedback":"no sources"}`
	v, err := parseVerdict(input)
	if err != nil {
		t.Fatal(err)
	}
	if v.Score != 0 {
		t.Errorf("score = %.2f, want 0 (lowest criterion: cites_sources=0)", v.Score)
	}
}

func TestAggregateVerdictMinAndClamp(t *testing.T) {
	// Per-criterion gating: the overall is the WEAKEST criterion (cites_sources=0),
	// not the mean, and not the model's submitted 0.96.
	v := aggregateVerdict(verdict{Criteria: map[string]criterionScore{
		"grounded":              {Score: 0.9},
		"no_fabrication":        {Score: 1.0},
		"answers_question":      {Score: 1.0},
		"internally_consistent": {Score: 0.9},
		"cites_sources":         {Score: 0.0},
	}, Score: 0.96})
	if v.Score != 0 {
		t.Errorf("score = %.2f, want 0 (lowest criterion)", v.Score)
	}
	// All-strong criteria → overall is the lowest of them.
	if got := aggregateVerdict(verdict{Criteria: map[string]criterionScore{
		"a": {Score: 0.8}, "b": {Score: 0.9}, "c": {Score: 0.7},
	}}).Score; got != 0.7 {
		t.Errorf("min: score = %v, want 0.7", got)
	}
	// No criteria: the submitted score is kept but clamped to [0,1].
	if got := aggregateVerdict(verdict{Score: 1.5}).Score; got != 1 {
		t.Errorf("clamp high: score = %v, want 1", got)
	}
	if got := aggregateVerdict(verdict{Score: -0.2}).Score; got != 0 {
		t.Errorf("clamp low: score = %v, want 0", got)
	}
}

// The rubric asks the judge for 0–10 integers; the pipeline works in 0–1. A
// verdict on the 0–10 scale (detected by any score > 1) must be divided by 10,
// so a perfect criterion (10) becomes 1.0 and the weakest drives the overall.
func TestParseVerdictNormalizes0To10Scale(t *testing.T) {
	input := `{"criteria":{"grounded":{"score":9},"no_fabrication":{"score":10},"answers_question":{"score":8},"internally_consistent":{"score":9},"cites_sources":{"score":6}},"score":8,"passed":true,"feedback":""}`
	v, err := parseVerdict(input)
	if err != nil {
		t.Fatal(err)
	}
	// Lowest criterion is cites_sources=6 → 0.6 after /10.
	if v.Score != 0.6 {
		t.Errorf("score = %v, want 0.6 (lowest criterion 6/10)", v.Score)
	}
	if g := v.Criteria["grounded"].Score; g != 0.9 {
		t.Errorf("grounded = %v, want 0.9 (9/10)", g)
	}
}

// normalizeScale must leave a verdict already on the 0–1 axis untouched (some
// models ignore the 0–10 instruction and answer in 0–1).
func TestNormalizeScaleLeaves0To1Untouched(t *testing.T) {
	v := verdict{Score: 0.9, Criteria: map[string]criterionScore{"a": {Score: 0.8}, "b": {Score: 0.5}}}
	normalizeScale(&v)
	if v.Score != 0.9 || v.Criteria["a"].Score != 0.8 || v.Criteria["b"].Score != 0.5 {
		t.Errorf("0–1 verdict was altered: %+v", v)
	}
}

// TestFoldDeterministic_RequireRetrievalHardFail: a retrieval agent that did
// ZERO web_search/web_fetch cannot pass the gate - regression for the live e2e
// 2026-07-05 hole where a worker wrote a question to the user as its answer
// text (no tool calls at all), citationScore abstained (nothing to grade), and
// the judge waved the "answer" through. Weakest-link must be 0, and the
// feedback must point at BOTH ways out (retrieve, or ask_user).
func TestFoldDeterministic_RequireRetrievalHardFail(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "Which city are you moving to?", workerActivity{}, Config{RequireRetrieval: true})
	if got.Score != 0 {
		t.Fatalf("score = %v, want 0 (weakest-link on grounded_in_retrieval)", got.Score)
	}
	c, ok := got.Criteria["grounded_in_retrieval"]
	if !ok || c.Score != 0 {
		t.Fatalf("grounded_in_retrieval criterion missing or nonzero: %+v", got.Criteria)
	}
	if !strings.Contains(c.Reason, "ask_user") {
		t.Errorf("feedback should name ask_user as the way out when blocked on the user; got %q", c.Reason)
	}
}

// TestFoldDeterministic_NoRetrievalOKForSynthesizer: a tool-less agent
// (RequireRetrieval=false) with no activity is NOT penalized - it legitimately
// re-cites upstream URLs (the pre-existing citationScore abstention stands).
func TestFoldDeterministic_NoRetrievalOKForSynthesizer(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "Combined findings: [x](https://ex.com/a).", workerActivity{}, Config{})
	if _, present := got.Criteria["grounded_in_retrieval"]; present {
		t.Fatal("grounded_in_retrieval applied to a non-retrieval agent")
	}
	if got.Score != 0.9 {
		t.Errorf("score = %v, want 0.9 (untouched)", got.Score)
	}
}

// TestFoldDeterministic_WorkspaceGroundingSatisfiesRetrieval: a coding node
// that consulted the repo on disk (clone and/or reads) instead of the web is
// grounded - grounded_in_retrieval must not fire on zero web activity alone.
func TestFoldDeterministic_WorkspaceGroundingSatisfiesRetrieval(t *testing.T) {
	for name, act := range map[string]workerActivity{
		"clone": {clonedRepos: []string{"https://github.com/org/repo"}, clonedDirs: []string{"repo"}},
		"reads": {paths: map[string]bool{"repo/main.go": true}},
	} {
		v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
		got := foldDeterministic(context.Background(), v, "The entrypoint is [main.go](repo/main.go).", act, Config{RequireRetrieval: true})
		if _, present := got.Criteria["grounded_in_retrieval"]; present {
			t.Errorf("%s: grounded_in_retrieval penalty applied despite workspace grounding", name)
		}
	}
}

// TestFoldDeterministic_RetrievalPresentNotPenalized: any recorded retrieval
// (even just search results seen) satisfies the grounding check; citation
// backing is then graded by citationScore as before.
func TestFoldDeterministic_RetrievalPresentNotPenalized(t *testing.T) {
	act := workerActivity{seen: map[string]string{"https://ex.com/a": "snippet"}}
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "Answer citing [x](https://ex.com/a).", act, Config{RequireRetrieval: true})
	if _, present := got.Criteria["grounded_in_retrieval"]; present {
		t.Fatal("grounded_in_retrieval penalty applied despite recorded retrieval")
	}
}

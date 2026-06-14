package vetting

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// scriptedModel returns each scripted response in turn, one per GenerateContent
// call, each as a turn-complete answer. It records how many times it was called.
type scriptedModel struct {
	name  string
	resps []string
	calls int
}

func (m *scriptedModel) Name() string { return m.name }

func (m *scriptedModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		text := ""
		if m.calls < len(m.resps) {
			text = m.resps[m.calls]
		} else if len(m.resps) > 0 {
			text = m.resps[len(m.resps)-1]
		}
		m.calls++
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}},
			TurnComplete: true,
		}, nil)
	}
}

// erroringModel always fails — stands in for a judge model that is down.
type erroringModel struct{ name string }

func (m erroringModel) Name() string { return m.name }

func (m erroringModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New("judge unavailable"))
	}
}

// scriptedPartsModel yields a scripted set of parts per GenerateContent call,
// each as a turn-complete response. It drives the agentic judge in tests by
// emitting tool calls (web verification, then submit_verdict).
type scriptedPartsModel struct {
	name  string
	turns [][]*genai.Part
	calls int
}

func (m *scriptedPartsModel) Name() string { return m.name }

func (m *scriptedPartsModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		i := m.calls
		if i >= len(m.turns) {
			i = len(m.turns) - 1
		}
		m.calls++
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: m.turns[i]},
			TurnComplete: true,
		}, nil)
	}
}

// judgeTurn is one scripted submit_verdict call from the test judge.
type judgeTurn struct {
	score    float64
	feedback string
	criteria map[string]any
}

// submitPart builds a submit_verdict tool call carrying t's verdict.
func submitPart(t judgeTurn) *genai.Part {
	args := map[string]any{"score": t.score, "feedback": t.feedback}
	if t.criteria != nil {
		args["criteria"] = t.criteria
	}
	return &genai.Part{FunctionCall: &genai.FunctionCall{ID: "v", Name: "submit_verdict", Args: args}}
}

// scriptedJudge returns a JudgeFactory whose judge calls submit_verdict once per
// round with the given verdicts (no web tools needed — the verdict is scripted).
func scriptedJudge(turns ...judgeTurn) JudgeFactory {
	parts := make([][]*genai.Part, len(turns))
	for i, t := range turns {
		parts[i] = []*genai.Part{submitPart(t)}
	}
	return NewJudgeFactory(&scriptedPartsModel{name: "j", turns: parts}, nil)
}

// gateResult collects what the trust gate streamed for one run, decoded through
// the stateful Translator (the new flat agent-run wire model).
type gateResult struct {
	starts    map[string]stream.AgentStartData // run_id → start
	verdicts  []stream.AgentCompleteData       // completed judge rounds (status == "")
	refines   []stream.AgentCompleteData       // self-refine completes
	toolCalls []stream.AgentToolCallData
	answer    string
}

// runGate wires the worker llmagent → gate → runner and runs one turn, returning
// the translated stream.
func runGate(t *testing.T, workerModel model.LLM, judge JudgeFactory, cfg Config) gateResult {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{
		Name:        "web-researcher",
		Description: "researches the web",
		Model:       workerModel,
		Instruction: "research",
	})
	if err != nil {
		t.Fatal(err)
	}
	gated, err := NewGatedAgent(worker, nil, judge, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "test",
		Agent:             gated,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "what is up"}}}

	res := gateResult{starts: map[string]stream.AgentStartData{}}
	translator := stream.NewTranslator()
	for ev, err := range r.Run(context.Background(), "u", "s1", content, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run error: %v", err)
		}
		for _, se := range translator.Event(ev) {
			switch d := se.Data.(type) {
			case stream.AgentStartData:
				res.starts[d.RunID] = d
			case stream.AgentCompleteData:
				switch {
				case d.Stage == stream.StageJudge && d.Status == "":
					res.verdicts = append(res.verdicts, d)
				case d.Stage == stream.StageSelfRefine:
					res.refines = append(res.refines, d)
				}
			case stream.AgentToolCallData:
				res.toolCalls = append(res.toolCalls, d)
			case stream.AgentTokenData:
				res.answer += d.Text
			}
		}
	}
	return res
}

func TestGatePassesOnFirstVerdict(t *testing.T) {
	worker := &scriptedModel{name: "w", resps: []string{"draft answer"}}
	judge := scriptedJudge(judgeTurn{score: 0.9, feedback: "good"})
	res := runGate(t, worker, judge, Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "r"})

	if len(res.verdicts) != 1 {
		t.Fatalf("verdicts = %+v, want 1", res.verdicts)
	}
	if !res.verdicts[0].Passed || res.verdicts[0].Round != 1 {
		t.Errorf("verdict[0] = %+v, want round 1 passed", res.verdicts[0])
	}
	if res.answer != "draft answer" {
		t.Errorf("answer = %q, want %q", res.answer, "draft answer")
	}
	if worker.calls != 1 {
		t.Errorf("worker model called %d times, want 1 (no revision)", worker.calls)
	}
}

func TestGateRevisesThenPasses(t *testing.T) {
	// Worker draft (call 1), then a revision (call 2). Judge fails round 1, passes
	// round 2.
	worker := &scriptedModel{name: "w", resps: []string{"draft answer", "revised answer"}}
	judge := scriptedJudge(
		judgeTurn{score: 0.4, feedback: "add sources"},
		judgeTurn{score: 0.8, feedback: "better"},
	)
	res := runGate(t, worker, judge, Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "r"})

	if len(res.verdicts) != 2 {
		t.Fatalf("verdicts = %+v, want 2", res.verdicts)
	}
	if res.verdicts[0].Passed || !res.verdicts[1].Passed {
		t.Errorf("verdicts = %+v, want fail then pass", res.verdicts)
	}
	if res.answer != "revised answer" {
		t.Errorf("answer = %q, want revised", res.answer)
	}
	if worker.calls != 2 {
		t.Errorf("worker model called %d times, want 2 (draft + 1 revision)", worker.calls)
	}
}

func TestGateStopsAtMaxRoundsStillAnswers(t *testing.T) {
	worker := &scriptedModel{name: "w", resps: []string{"a", "b", "c"}}
	judge := scriptedJudge(judgeTurn{score: 0.1, feedback: "no"})
	res := runGate(t, worker, judge, Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "r"})

	if len(res.verdicts) != 2 {
		t.Fatalf("verdicts = %+v, want 2 (max rounds)", res.verdicts)
	}
	if res.verdicts[0].Passed || res.verdicts[1].Passed {
		t.Errorf("verdicts = %+v, want both failing", res.verdicts)
	}
	// Still returns the last revision rather than nothing.
	if res.answer == "" {
		t.Error("answer is empty, want the last revision surfaced anyway")
	}
}

func TestGateRecoversFromEmptyAnswer(t *testing.T) {
	// The worker ends round 0 with no answer text (it tried to call another tool,
	// or thought into the void and stopped). The gate must re-invoke it with a
	// finalize write-up pass and surface the recovered answer — not skip straight
	// to an empty result.
	worker := &scriptedModel{name: "w", resps: []string{"", "recovered answer"}}
	judge := scriptedJudge(judgeTurn{score: 0.9, feedback: "good"})
	res := runGate(t, worker, judge, Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "r"})

	if res.answer != "recovered answer" {
		t.Errorf("answer = %q, want %q (recovered via finalize pass)", res.answer, "recovered answer")
	}
	if worker.calls != 2 {
		t.Errorf("worker model called %d times, want 2 (empty round 0 + 1 finalize retry)", worker.calls)
	}
	// The recovered answer is non-empty, so the judge still runs and scores it.
	if len(res.verdicts) != 1 || !res.verdicts[0].Passed {
		t.Errorf("verdicts = %+v, want one passing (judge runs on the recovered answer)", res.verdicts)
	}
}

func TestGateEmptyAnswerExhaustsRetriesAndSkipsJudge(t *testing.T) {
	// The worker never produces answer text. The gate retries the finalize pass up
	// to maxEmptyRetries times, then surfaces the empty answer without burning a
	// (slow, agentic) judge round scoring nothing.
	worker := &scriptedModel{name: "w", resps: []string{""}}
	judge := scriptedJudge(judgeTurn{score: 0.9, feedback: "unused"})
	res := runGate(t, worker, judge, Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "r"})

	if res.answer != "" {
		t.Errorf("answer = %q, want empty (every finalize retry came back empty)", res.answer)
	}
	if want := 1 + maxEmptyRetries; worker.calls != want {
		t.Errorf("worker model called %d times, want %d (round 0 + %d finalize retries)", worker.calls, want, maxEmptyRetries)
	}
	if len(res.verdicts) != 0 {
		t.Errorf("verdicts = %+v, want none (judge skipped on an empty answer)", res.verdicts)
	}
}

func TestGateSelfRefineEmitsAndRevises(t *testing.T) {
	// Worker draft (call 1), self-refine revision (call 2). Then judge passes.
	worker := &scriptedModel{name: "w", resps: []string{"draft answer", "self-refined answer"}}
	judge := scriptedJudge(judgeTurn{score: 0.9, feedback: "ok"})
	res := runGate(t, worker, judge, Config{JudgeRounds: 2, Threshold: 0.7, SelfCritiqueRounds: 1, Rubric: "r"})

	if len(res.refines) != 1 || !res.refines[0].Changed {
		t.Errorf("refines = %+v, want one changed self_refine", res.refines)
	}
	if res.answer != "self-refined answer" {
		t.Errorf("answer = %q, want self-refined", res.answer)
	}
}

func TestGateJudgeUnavailableSurfacesAnswer(t *testing.T) {
	// When the judge model errors, the gate must emit a judge_unavailable event
	// and surface the answer anyway (quality-cannot-be-guaranteed degradation).
	worker, err := llmagent.New(llmagent.Config{Name: "web-researcher", Description: "d", Model: &scriptedModel{name: "w", resps: []string{"best effort answer"}}, Instruction: "x"})
	if err != nil {
		t.Fatal(err)
	}
	gated, err := NewGatedAgent(worker, nil, NewJudgeFactory(erroringModel{name: "j"}, nil), Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "r"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: gated, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "q"}}}

	var answer string
	var unavailable []stream.AgentCompleteData
	translator := stream.NewTranslator()
	for ev, err := range r.Run(context.Background(), "u", "s1", content, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("unexpected run error: %v", err)
		}
		for _, se := range translator.Event(ev) {
			switch d := se.Data.(type) {
			case stream.AgentTokenData:
				answer += d.Text
			case stream.AgentCompleteData:
				if d.Stage == stream.StageJudge && d.Status == "unavailable" {
					unavailable = append(unavailable, d)
				}
			}
		}
	}
	if len(unavailable) == 0 {
		t.Fatal("expected a judge run completed with status=unavailable")
	}
	if unavailable[0].Round != 1 {
		t.Errorf("judge unavailable round = %d, want 1", unavailable[0].Round)
	}
	if unavailable[0].Reason == "" {
		t.Error("judge unavailable reason should be non-empty")
	}
	if answer != "best effort answer" {
		t.Errorf("answer = %q, want surfaced despite judge error", answer)
	}
}

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
		fetched: map[string]fetchRecord{"https://ex.com/fetched-page": {}},
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

func TestCitationScoreNormalizesAnchorsAndSlashes(t *testing.T) {
	// Cited URL differs from the fetched one only by a trailing slash and a #anchor
	// and host casing — should still score a 1.0 exact-fetch match.
	answer := "See [x](https://Ex.com/Page/#section)."
	act := workerActivity{fetched: map[string]fetchRecord{"https://ex.com/Page": {}}}
	score, _, ok := citationScore(answer, act)
	if !ok || score != 1.0 {
		t.Errorf("score=%.2f ok=%v, want 1.0 true (anchor/slash/case normalized)", score, ok)
	}
}

func TestCitationScoreNoCitations(t *testing.T) {
	_, _, ok := citationScore("A plain answer with no links.", workerActivity{})
	if ok {
		t.Error("citationScore ok=true for an answer with no URLs, want false")
	}
}

func TestCitationScoreSkippedWithoutRetrieval(t *testing.T) {
	// A non-web agent (synthesizer) does no fetch/search, so its citations can't be
	// graded — ok=false leaves the model's cites_sources in place.
	answer := "Per the research, [x](https://ex.com/a)."
	if _, _, ok := citationScore(answer, workerActivity{}); ok {
		t.Error("citationScore ok=true with no retrieval recorded, want false (skip override)")
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
	v, err := parseVerdict("```json\n{\"score\": 1.5, \"passed\": true, \"feedback\": \"x\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if v.Score != 1 { // clamped to [0,1]
		t.Errorf("score = %v, want clamped to 1", v.Score)
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

// TestGateJudgeVerifiesAgentically proves the judge runs a tool loop before
// scoring (requirement: agentic, not one-shot) and that its tool activity
// streams to the consumer. The judge calls a verification tool, then submits.
func TestGateJudgeVerifiesAgentically(t *testing.T) {
	lookup, err := functiontool.New(functiontool.Config{
		Name:        "lookup",
		Description: "verify a claim",
	}, func(_ tool.Context, _ struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	judgeModel := &scriptedPartsModel{name: "j", turns: [][]*genai.Part{
		{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "lookup", Args: map[string]any{}}}},
		{submitPart(judgeTurn{score: 0.9, feedback: "verified"})},
	}}
	judge := NewJudgeFactory(judgeModel, []tool.Tool{lookup})

	worker := &scriptedModel{name: "w", resps: []string{"draft answer"}}
	res := runGate(t, worker, judge, Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "r"})

	if len(res.verdicts) != 1 || !res.verdicts[0].Passed {
		t.Fatalf("verdicts = %+v, want one passing", res.verdicts)
	}
	var sawLookup bool
	for _, tc := range res.toolCalls {
		if tc.Name == "lookup" {
			sawLookup = true
			// The tool call belongs to the judge run (correlate via its agent_start).
			if st, ok := res.starts[tc.RunID]; !ok || st.Stage != stream.StageJudge {
				t.Errorf("lookup tool_call run %q = %+v, want a judge-stage run", tc.RunID, st)
			}
		}
	}
	if !sawLookup {
		t.Errorf("expected the judge's lookup tool_call to stream; toolCalls = %+v", res.toolCalls)
	}
	if res.answer != "draft answer" {
		t.Errorf("answer = %q, want %q", res.answer, "draft answer")
	}
}

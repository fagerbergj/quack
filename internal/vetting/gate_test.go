package vetting

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
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

// gateResult collects what the trust gate streamed for one run.
type gateResult struct {
	verdicts []stream.JudgeVerdictData
	refines  []stream.SelfRefineData
	answer   string
}

// runGate wires the worker llmagent → gate → runner and runs one turn, returning
// the translated stream.
func runGate(t *testing.T, workerModel, judge model.LLM, cfg Config) gateResult {
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
	gated, err := NewGatedAgent(worker, workerModel, judge, cfg)
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

	var res gateResult
	for ev, err := range r.Run(context.Background(), "u", "s1", content, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run error: %v", err)
		}
		for _, se := range stream.Translate(ev) {
			switch d := se.Data.(type) {
			case stream.JudgeVerdictData:
				res.verdicts = append(res.verdicts, d)
			case stream.SelfRefineData:
				res.refines = append(res.refines, d)
			case stream.TokenData:
				res.answer += d.Text
			}
		}
	}
	return res
}

func TestGatePassesOnFirstVerdict(t *testing.T) {
	worker := &scriptedModel{name: "w", resps: []string{"draft answer"}}
	judge := &scriptedModel{name: "j", resps: []string{`{"score":0.9,"passed":true,"feedback":"good"}`}}
	res := runGate(t, worker, judge, Config{MaxRounds: 2, Threshold: 0.7, Rubric: "r"})

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
	judge := &scriptedModel{name: "j", resps: []string{
		`{"score":0.4,"passed":false,"feedback":"add sources"}`,
		`{"score":0.8,"passed":true,"feedback":"better"}`,
	}}
	res := runGate(t, worker, judge, Config{MaxRounds: 2, Threshold: 0.7, Rubric: "r"})

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
	judge := &scriptedModel{name: "j", resps: []string{`{"score":0.1,"passed":false,"feedback":"no"}`}}
	res := runGate(t, worker, judge, Config{MaxRounds: 2, Threshold: 0.7, Rubric: "r"})

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

func TestGateSelfRefineEmitsAndRevises(t *testing.T) {
	// Worker draft (call 1), self-refine revision (call 2). Then judge passes.
	worker := &scriptedModel{name: "w", resps: []string{"draft answer", "self-refined answer"}}
	judge := &scriptedModel{name: "j", resps: []string{`{"score":0.9,"passed":true,"feedback":"ok"}`}}
	res := runGate(t, worker, judge, Config{MaxRounds: 2, Threshold: 0.7, SelfRefine: true, Rubric: "r"})

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
	gated, err := NewGatedAgent(worker, &scriptedModel{name: "w", resps: []string{"x"}}, erroringModel{name: "j"}, Config{MaxRounds: 2, Threshold: 0.7, Rubric: "r"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: gated, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "q"}}}

	var answer string
	var unavailable []stream.JudgeUnavailableData
	for ev, err := range r.Run(context.Background(), "u", "s1", content, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("unexpected run error: %v", err)
		}
		for _, se := range stream.Translate(ev) {
			switch d := se.Data.(type) {
			case stream.TokenData:
				answer += d.Text
			case stream.JudgeUnavailableData:
				unavailable = append(unavailable, d)
			}
		}
	}
	if len(unavailable) == 0 {
		t.Error("expected a judge_unavailable event")
	}
	if unavailable[0].Round != 1 {
		t.Errorf("judge_unavailable round = %d, want 1", unavailable[0].Round)
	}
	if unavailable[0].Reason == "" {
		t.Error("judge_unavailable reason should be non-empty")
	}
	if answer != "best effort answer" {
		t.Errorf("answer = %q, want surfaced despite judge error", answer)
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
	// cites_sources=0 → hard cap at 0.40
	if v.Score > 0.40 {
		t.Errorf("score = %.2f, want ≤ 0.40 (cites_sources=0 hard cap)", v.Score)
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

func TestParseVerdictCitesCap(t *testing.T) {
	// Well-formed G-Eval verdict; cites_sources=0 should cap the mean.
	input := `{"criteria":{"grounded":{"score":0.9},"no_fabrication":{"score":1.0},"answers_question":{"score":1.0},"internally_consistent":{"score":0.9},"cites_sources":{"score":0.0}},"score":0.96,"passed":true,"feedback":"no sources"}`
	v, err := parseVerdict(input)
	if err != nil {
		t.Fatal(err)
	}
	if v.Score > 0.40 {
		t.Errorf("score = %.2f, want ≤ 0.40 (cites_sources=0 hard cap)", v.Score)
	}
}

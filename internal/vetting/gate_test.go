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

func TestGateFailsClosedOnJudgeError(t *testing.T) {
	// When the judge model errors, the gate must surface the error and NOT emit
	// the un-vetted answer (which the runner would otherwise persist).
	worker, err := llmagent.New(llmagent.Config{Name: "web-researcher", Description: "d", Model: &scriptedModel{name: "w", resps: []string{"un-vetted draft"}}, Instruction: "x"})
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

	var sawError bool
	var answer string
	for ev, err := range r.Run(context.Background(), "u", "s1", content, agent.RunConfig{}) {
		if err != nil {
			sawError = true
			continue
		}
		for _, se := range stream.Translate(ev) {
			if d, ok := se.Data.(stream.TokenData); ok {
				answer += d.Text
			}
		}
	}
	if !sawError {
		t.Error("expected a judge error to surface")
	}
	if answer != "" {
		t.Errorf("un-vetted answer leaked on judge error: %q", answer)
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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/vetting"
)

// content builds one genai.Content for a fixture turn.
func content(role string, parts ...*genai.Part) *genai.Content {
	return &genai.Content{Role: role, Parts: parts}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// buildFixtureBundle writes a one-node recording with a single judged round:
// a worker round that fetched one URL and cited it, and the judge's recorded
// per-criterion score - shaped exactly as bundle.Load reads (a bare entries.jsonl of ledger.Entry lines).
func buildFixtureBundle(t *testing.T, recordedScore float64) string {
	t.Helper()
	now := time.Now()

	question := content(genai.RoleUser, &genai.Part{Text: "What backs the claim?"})
	task := content(genai.RoleUser, &genai.Part{Text: "Research the claim."})
	fetchCall := content(genai.RoleModel, &genai.Part{FunctionCall: &genai.FunctionCall{ID: "1", Name: "web_fetch", Args: map[string]any{"url": "https://example.com/a"}}})
	fetchResp := content(genai.RoleUser, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "1", Name: "web_fetch", Response: map[string]any{"results": []any{map[string]any{"url": "https://example.com/a"}}}}})
	answer := content(genai.RoleModel, &genai.Part{Text: "See [source](https://example.com/a) for details."})

	workerInput := mustJSON(t, []*genai.Content{task, fetchCall, fetchResp})
	workerOutput := mustJSON(t, answer)

	entries := []ledger.Entry{
		{Seq: 1, ChatID: "chat-1", Kind: ledger.KindLLMCall, At: now,
			Payload: mustPayload(t, ledger.LLMCallPayload{Input: mustJSON(t, []*genai.Content{question}), Output: mustJSON(t, content(genai.RoleModel, &genai.Part{Text: "answer"}))})},
		{Seq: 2, ChatID: "chat-1", NodeID: "node-1", Agent: "web-researcher", Round: "worker-r0", Kind: ledger.KindLLMCall, At: now.Add(time.Second),
			Payload: mustPayload(t, ledger.LLMCallPayload{Input: workerInput, Output: workerOutput})},
		{Seq: 3, ChatID: "chat-1", NodeID: "node-1", Round: "judge-r1", Kind: ledger.KindEvalScore, At: now.Add(2 * time.Second),
			Payload: mustPayload(t, ledger.EvalScorePayload{Criterion: "cites_sources", Score: recordedScore})},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "entries.jsonl")
	var buf bytes.Buffer
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return path
}

func mustPayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// rubricFile writes a minimal rubric.yaml with cites_sources' pass mark at pass.
func rubricFile(t *testing.T, pass float64) string {
	t.Helper()
	doc := `scale:
  min: 0
  max: 3
  pass: 2
criteria:
  cites_sources:
    definition: existence check
    bands:
    - {min: 0.75, max: 1.0, meaning: backed}
    - {min: 0.25, max: 0.74, meaning: maybe}
    - {min: 0.0, max: 0.24, meaning: fabricated}
    deterministic: true
    fix: fetch it
    scale: {min: 0, max: 1, pass: ` + fmtFloat(pass) + `}
`
	path := filepath.Join(t.TempDir(), "rubric.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write rubric: %v", err)
	}
	return path
}

func fmtFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func fixtureConfig() *config.Config {
	return &config.Config{Agents: map[string]config.AgentConfig{"web-researcher": {Bundle: "agents/web-researcher"}}}
}

// fakeLLM fails the test if ever generated from - the model.LLM instrumented judgePoisonFactory wraps.
type fakeLLM struct{ t *testing.T }

func (f fakeLLM) Name() string { return "fake" }
func (f fakeLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.t.Fatal("judge model must never be called by --deterministic-only")
		yield(nil, nil)
	}
}

func TestRunJudgeReplayReproducesRecordedDeterministicScore(t *testing.T) {
	ctx := context.Background()
	bundlePath := buildFixtureBundle(t, 1.0) // fetched URL == cited URL ⇒ citationScore computes 1.0
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	opts := ReplayOptions{RubricPath: rubricFile(t, 0.85), DeterministicOnly: true}

	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, &buf, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}

	var reports []ReplayRoundReport
	if err := json.Unmarshal(buf.Bytes(), &reports); err != nil {
		t.Fatalf("decode json: %v\n%s", err, buf.String())
	}
	if len(reports) != 1 {
		t.Fatalf("got %d round report(s), want 1: %+v", len(reports), reports)
	}
	r := reports[0]
	if r.Skipped != "" {
		t.Fatalf("round skipped: %s", r.Skipped)
	}
	if len(r.Criteria) != 1 || r.Criteria[0].Name != "cites_sources" {
		t.Fatalf("criteria = %+v, want exactly cites_sources", r.Criteria)
	}
	c := r.Criteria[0]
	if c.RecordedScore != 1.0 || c.ReplayedScore != 1.0 {
		t.Errorf("recorded=%.2f replayed=%.2f, want both 1.0", c.RecordedScore, c.ReplayedScore)
	}
	if !c.RecordedPassed || !c.ReplayedPassed || c.Flipped {
		t.Errorf("recordedPassed=%v replayedPassed=%v flipped=%v, want pass/pass/no-flip", c.RecordedPassed, c.ReplayedPassed, c.Flipped)
	}
}

func TestRunJudgeReplayRubricPassMarkFlip(t *testing.T) {
	ctx := context.Background()
	// Recorded below a low pass mark's true value (0.8) but the code always
	// replays this fixture's citation at 1.0 - a stand-in for "the rubric or
	// code changed since this round was recorded".
	bundlePath := buildFixtureBundle(t, 0.8)
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}

	lowMark := ReplayOptions{RubricPath: rubricFile(t, 0.5), DeterministicOnly: true}
	var buf bytes.Buffer
	if code := RunJudgeReplay(ctx, fixtureConfig(), sess, lowMark, nil, &buf, false); code != 0 {
		t.Fatalf("low pass mark: exit code = %d, want 0 (no flip): %s", code, buf.String())
	}

	raisedMark := ReplayOptions{RubricPath: rubricFile(t, 0.85), DeterministicOnly: true}
	buf.Reset()
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, raisedMark, nil, &buf, false)
	if code == 0 {
		t.Fatalf("raised pass mark: exit code = 0, want non-zero (a flip): %s", buf.String())
	}
	if !strings.Contains(buf.String(), "!") {
		t.Errorf("raised pass mark output has no flip marker:\n%s", buf.String())
	}
}

func TestRunJudgeReplayDeterministicOnlyNeverTouchesJudge(t *testing.T) {
	ctx := context.Background()
	bundlePath := buildFixtureBundle(t, 1.0)
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	calls := 0
	judge := vetting.CountingJudgeFactory(vetting.NewJudgeFactory(fakeLLM{t: t}, nil, nil), &calls)

	opts := ReplayOptions{RubricPath: rubricFile(t, 0.85), DeterministicOnly: true}
	var buf bytes.Buffer
	RunJudgeReplay(ctx, fixtureConfig(), sess, opts, judge, &buf, false)
	if calls != 0 {
		t.Fatalf("judge factory invoked %d time(s) under --deterministic-only, want 0", calls)
	}
}

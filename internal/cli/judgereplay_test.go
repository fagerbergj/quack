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

func mustPayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

func writeEntries(t *testing.T, entries []ledger.Entry) string {
	t.Helper()
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

// buildFixtureBundle writes a one-node recording: a worker round (workerRound
// names its round id) that fetched and cited one URL, plus recorded's eval.score entries.
func buildFixtureBundle(t *testing.T, workerRound string, recorded map[string]float64) string {
	t.Helper()
	now := time.Now()

	task := content(genai.RoleUser, &genai.Part{Text: "Research the claim."})
	fetchCall := content(genai.RoleModel, &genai.Part{FunctionCall: &genai.FunctionCall{ID: "1", Name: "web_fetch"}})
	fetchResp := content(genai.RoleUser, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "1", Name: "web_fetch",
		Response: map[string]any{"results": []any{map[string]any{"url": "https://example.com/a"}}}}})
	answer := content(genai.RoleModel, &genai.Part{Text: "See [source](https://example.com/a) for details."})

	workerInput := mustJSON(t, []*genai.Content{task, fetchCall, fetchResp})
	workerOutput := mustJSON(t, answer)

	entries := []ledger.Entry{
		{Seq: 1, ChatID: "chat-1", NodeID: "node-1", Agent: "web-researcher", Round: workerRound, Kind: ledger.KindLLMCall, At: now.Add(time.Second),
			Payload: mustPayload(t, ledger.LLMCallPayload{Input: workerInput, Output: workerOutput})},
	}
	seq := int64(2)
	for name, score := range recorded {
		entries = append(entries, ledger.Entry{Seq: seq, ChatID: "chat-1", NodeID: "node-1", Round: "judge-r1", Kind: ledger.KindEvalScore, At: now.Add(2 * time.Second),
			Payload: mustPayload(t, ledger.EvalScorePayload{Criterion: name, Score: score})})
		seq++
	}
	return writeEntries(t, entries)
}

// rubricFile writes a minimal rubric.yaml with cites_sources' definition
// marked distinctively (to prove it reaches cfg.Rubric) and its pass mark at pass.
func rubricFile(t *testing.T, marker string, pass float64) string {
	t.Helper()
	doc := `scale:
  min: 0
  max: 3
  pass: 2
criteria:
  cites_sources:
    definition: ` + marker + `
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

// fixtureConfig sets a real gate Threshold (0.7) - RunJudgeReplay's pass/fail
// decision - independent of whatever pass mark a test's rubric.yaml declares.
func fixtureConfig() *config.Config {
	return &config.Config{
		Agents: map[string]config.AgentConfig{"web-researcher": {Bundle: "agents/web-researcher"}},
		Gates:  config.GatesConfig{Judge: config.JudgeConfig{Threshold: 0.7}},
	}
}

// fakeLLM fails the test if ever generated from - the model.LLM a poisoned
// judge factory wraps for a --deterministic-only test.
type fakeLLM struct{ t *testing.T }

func (f fakeLLM) Name() string { return "fake" }
func (f fakeLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.t.Fatal("judge model must never be called by --deterministic-only")
		yield(nil, nil)
	}
}

func decodeReports(t *testing.T, buf *bytes.Buffer) []ReplayRoundReport {
	t.Helper()
	var reports []ReplayRoundReport
	if err := json.Unmarshal(buf.Bytes(), &reports); err != nil {
		t.Fatalf("decode json: %v\n%s", err, buf.String())
	}
	return reports
}

func TestRunJudgeReplayReproducesRecordedDeterministicScore(t *testing.T) {
	ctx := context.Background()
	// Above threshold both sides; a rubric mark that would (wrongly) fail
	// this score is ignored for the verdict.
	bundlePath := buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	opts := ReplayOptions{RubricPath: rubricFile(t, "existence check", 0.95), DeterministicOnly: true}

	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, &buf, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, buf.String())
	}
	reports := decodeReports(t, &buf)
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
	if c.RubricMark != 0.95 {
		t.Errorf("RubricMark = %v, want the rubric's own 0.95 mark reported, unused for pass/fail", c.RubricMark)
	}
}

// TestRunJudgeReplayThresholdDecidesPassFail proves pass/fail uses
// cfg.Threshold (the live gate's own decision), never the rubric's own mark.
func TestRunJudgeReplayThresholdDecidesPassFail(t *testing.T) {
	ctx := context.Background()
	// Recorded below cfg.Threshold (0.7); the fixture's citation always
	// replays at 1.0 (fetched URL == cited URL), above threshold.
	bundlePath := buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 0.6})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	// The rubric's own mark (0.30) would call 0.6 a pass if it decided
	// anything - proving it doesn't.
	opts := ReplayOptions{RubricPath: rubricFile(t, "existence check", 0.30), DeterministicOnly: true}
	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, &buf, true)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero (a flip): %s", buf.String())
	}
	reports := decodeReports(t, &buf)
	c := reports[0].Criteria[0]
	if c.Threshold != 0.7 {
		t.Errorf("Threshold = %v, want cfg.Gates.Judge.Threshold (0.7)", c.Threshold)
	}
	if c.RecordedPassed {
		t.Errorf("recorded 0.6 < threshold 0.7 should fail, got RecordedPassed=true")
	}
	if !c.ReplayedPassed {
		t.Errorf("replayed 1.0 >= threshold 0.7 should pass, got ReplayedPassed=false")
	}
	if !c.Flipped {
		t.Error("recorded fail vs. replayed pass across the same threshold should flip")
	}
	if c.RubricMark != 0.30 {
		t.Errorf("RubricMark = %v, want the rubric's own 0.30 mark reported separately", c.RubricMark)
	}
}

func TestRunJudgeReplayRecordedOnlyCriterionIsAFlip(t *testing.T) {
	ctx := context.Background()
	// "grounded" is judge-scored, never produced by deterministic-only replay -
	// a dropped once-recorded criterion must flip, not vanish silently.
	bundlePath := buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0, "grounded": 1.0})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	opts := ReplayOptions{RubricPath: rubricFile(t, "existence check", 0.85), DeterministicOnly: true}
	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, &buf, true)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero (recorded-only criterion): %s", buf.String())
	}
	reports := decodeReports(t, &buf)
	var found *ReplayCriterionReport
	for i, c := range reports[0].Criteria {
		if c.Name == "grounded" {
			found = &reports[0].Criteria[i]
		}
	}
	if found == nil {
		t.Fatalf("criteria = %+v, want a recorded-only \"grounded\" entry", reports[0].Criteria)
	}
	if !found.HasRecorded || found.HasReplayed || !found.Flipped {
		t.Errorf("grounded = %+v, want HasRecorded=true HasReplayed=false Flipped=true", found)
	}
}

func TestRunJudgeReplayDeterministicOnlyNeverTouchesJudge(t *testing.T) {
	ctx := context.Background()
	bundlePath := buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	calls := 0
	judge := vetting.CountingJudgeFactory(vetting.NewJudgeFactory(fakeLLM{t: t}, nil, nil), &calls)

	opts := ReplayOptions{RubricPath: rubricFile(t, "existence check", 0.85), DeterministicOnly: true}
	var buf bytes.Buffer
	RunJudgeReplay(ctx, fixtureConfig(), sess, opts, judge, nil, &buf, false)
	if calls != 0 {
		t.Fatalf("judge factory invoked %d time(s) under --deterministic-only, want 0", calls)
	}
}

func TestRunJudgeReplayNodeAndRoundFilters(t *testing.T) {
	ctx := context.Background()
	bundlePath := buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	rubricPath := rubricFile(t, "existence check", 0.85)

	var buf bytes.Buffer
	RunJudgeReplay(ctx, fixtureConfig(), sess, ReplayOptions{Node: "node-1", RubricPath: rubricPath, DeterministicOnly: true}, nil, nil, &buf, true)
	if reports := decodeReports(t, &buf); len(reports) != 1 {
		t.Fatalf("--node node-1: %d report(s), want 1: %s", len(reports), buf.String())
	}

	buf.Reset()
	RunJudgeReplay(ctx, fixtureConfig(), sess, ReplayOptions{Node: "no-such-node", RubricPath: rubricPath, DeterministicOnly: true}, nil, nil, &buf, true)
	if reports := decodeReports(t, &buf); len(reports) != 0 {
		t.Fatalf("--node no-such-node: %d report(s), want 0: %s", len(reports), buf.String())
	}

	buf.Reset()
	RunJudgeReplay(ctx, fixtureConfig(), sess, ReplayOptions{Round: 2, RubricPath: rubricPath, DeterministicOnly: true}, nil, nil, &buf, true)
	if reports := decodeReports(t, &buf); len(reports) != 0 {
		t.Fatalf("--round 2 (only judge-r1 recorded): %d report(s), want 0: %s", len(reports), buf.String())
	}
}

func TestRunJudgeReplaySkipsRoundWithNoWorkerAnswer(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	path := writeEntries(t, []ledger.Entry{
		{Seq: 1, ChatID: "chat-1", NodeID: "node-1", Round: "judge-r1", Kind: ledger.KindEvalScore, At: now,
			Payload: mustPayload(t, ledger.EvalScorePayload{Criterion: "cites_sources", Score: 1.0})},
	})
	sess, err := bundle.Load(path)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, ReplayOptions{DeterministicOnly: true}, nil, nil, &buf, false)
	if code == 0 {
		t.Errorf("exit code = 0, want non-zero for an unrebuildable round")
	}
	if !strings.Contains(buf.String(), "skipped:") {
		t.Errorf("output has no skipped note:\n%s", buf.String())
	}
}

// TestWorkerTurnsForMatchesAnyRoundID proves round pairing is chronological,
// not a "worker-rN" string match - any round id form must work like a draft's.
func TestWorkerTurnsForMatchesAnyRoundID(t *testing.T) {
	ctx := context.Background()
	bundlePath := buildFixtureBundle(t, "worker-cont3-s2", map[string]float64{"cites_sources": 1.0})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	opts := ReplayOptions{RubricPath: rubricFile(t, "existence check", 0.85), DeterministicOnly: true}
	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, &buf, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0: %s", code, buf.String())
	}
	reports := decodeReports(t, &buf)
	if len(reports) != 1 || reports[0].Skipped != "" {
		t.Fatalf("non-standard round id %q: reports = %+v, want it rebuilt like worker-r0", "worker-cont3-s2", reports)
	}
}

// TestActivityScopeIncludesOtherNodesBeforeJudgeRound proves activity comes
// from the whole session (live's ctx.Session() scan), not just this node's.
func TestActivityScopeIncludesOtherNodesBeforeJudgeRound(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	// node-A fetches the URL; node-B's answer cites it but never fetches
	// anything itself - only cross-node activity scope backs the citation.
	fetchCall := content(genai.RoleModel, &genai.Part{FunctionCall: &genai.FunctionCall{ID: "1", Name: "web_fetch"}})
	fetchResp := content(genai.RoleUser, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "1", Name: "web_fetch",
		Response: map[string]any{"results": []any{map[string]any{"url": "https://example.com/shared"}}}}})
	nodeAInput := mustJSON(t, []*genai.Content{content(genai.RoleUser, &genai.Part{Text: "fetch it"}), fetchCall, fetchResp})
	nodeAOutput := mustJSON(t, content(genai.RoleModel, &genai.Part{Text: "fetched"}))

	nodeBTask := content(genai.RoleUser, &genai.Part{Text: "cite the shared source"})
	nodeBAnswer := content(genai.RoleModel, &genai.Part{Text: "See [source](https://example.com/shared)."})
	nodeBInput := mustJSON(t, []*genai.Content{nodeBTask})
	nodeBOutput := mustJSON(t, nodeBAnswer)

	path := writeEntries(t, []ledger.Entry{
		{Seq: 1, ChatID: "chat-1", NodeID: "node-A", Agent: "web-researcher", Round: "worker-r0", Kind: ledger.KindLLMCall, At: now,
			Payload: mustPayload(t, ledger.LLMCallPayload{Input: nodeAInput, Output: nodeAOutput})},
		{Seq: 2, ChatID: "chat-1", NodeID: "node-B", Agent: "web-researcher", Round: "worker-r0", Kind: ledger.KindLLMCall, At: now.Add(time.Second),
			Payload: mustPayload(t, ledger.LLMCallPayload{Input: nodeBInput, Output: nodeBOutput})},
		{Seq: 3, ChatID: "chat-1", NodeID: "node-B", Round: "judge-r1", Kind: ledger.KindEvalScore, At: now.Add(2 * time.Second),
			Payload: mustPayload(t, ledger.EvalScorePayload{Criterion: "cites_sources", Score: 1.0})},
	})
	sess, err := bundle.Load(path)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	opts := ReplayOptions{Node: "node-B", RubricPath: rubricFile(t, "existence check", 0.85), DeterministicOnly: true}
	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, &buf, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (node-A's fetch should back node-B's citation): %s", code, buf.String())
	}
	reports := decodeReports(t, &buf)
	if len(reports) != 1 || len(reports[0].Criteria) != 1 || reports[0].Criteria[0].ReplayedScore != 1.0 {
		t.Fatalf("reports = %+v, want cites_sources=1.0 backed by node-A's fetch", reports)
	}
}

func TestRubricConfigForRendersRubricIntoCfgRubric(t *testing.T) {
	rubricPath := rubricFile(t, "a distinctive marker for this test", 0.85)
	gc, err := RubricConfigFor(context.Background(), fixtureConfig(), "web-researcher", rubricPath, nil)
	if err != nil {
		t.Fatalf("RubricConfigFor: %v", err)
	}
	if !strings.Contains(gc.Rubric, "a distinctive marker for this test") {
		t.Errorf("cfg.Rubric = %q, want it to carry the working-copy rubric's own text (buildJudgePrompt embeds cfg.Rubric verbatim)", gc.Rubric)
	}
}

func TestRubricConfigForUnknownAgent(t *testing.T) {
	if _, err := RubricConfigFor(context.Background(), &config.Config{}, "no-such-agent", "", nil); err == nil {
		t.Fatal("RubricConfigFor for an agent absent from quack.yaml: err = nil, want an error")
	}
}

func TestRubricConfigForEmptyAgentName(t *testing.T) {
	if _, err := RubricConfigFor(context.Background(), &config.Config{}, "", "", nil); err == nil {
		t.Fatal("RubricConfigFor with no recorded worker agent: err = nil, want a clear error")
	}
}

func TestBuildReplayJudgeDisabledReturnsNil(t *testing.T) {
	judge, err := BuildReplayJudge(&config.Config{}, func(config.ProviderConfig, string) (model.LLM, error) {
		t.Fatal("newModel called though gates.judge is disabled")
		return nil, nil
	})
	if err != nil || judge != nil {
		t.Errorf("BuildReplayJudge with no judge configured = (%v, %v), want (nil, nil)", judge, err)
	}
}

func TestBuildReplayJudgeUnknownProvider(t *testing.T) {
	cfg := &config.Config{Gates: config.GatesConfig{Judge: config.JudgeConfig{Model: "m", MaxRounds: 1, Provider: "missing"}}}
	if _, err := BuildReplayJudge(cfg, func(config.ProviderConfig, string) (model.LLM, error) { return nil, nil }); err == nil {
		t.Fatal("BuildReplayJudge with an unknown provider: err = nil, want an error")
	}
}

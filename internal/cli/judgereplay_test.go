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
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, false, &buf, true)
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
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, false, &buf, true)
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

// TestCompareCriteriaRecordedOnlyIsAFlipOnlyWhenJudgeRan: an unreproduced
// recorded criterion flips only when the judge actually ran this replay.
func TestCompareCriteriaRecordedOnlyIsAFlipOnlyWhenJudgeRan(t *testing.T) {
	res := vetting.ReplayRoundResult{Threshold: 0.7}
	recorded := map[string]float64{"grounded": 1.0}

	det := compareCriteria(res, recorded, false)
	if len(det) != 1 || det[0].Flipped {
		t.Errorf("deterministic-only: grounded = %+v, want present, Flipped=false", det)
	}
	if !det[0].HasRecorded || det[0].HasReplayed {
		t.Errorf("deterministic-only: grounded = %+v, want HasRecorded=true HasReplayed=false", det[0])
	}

	full := compareCriteria(res, recorded, true)
	if len(full) != 1 || !full[0].Flipped {
		t.Errorf("judge ran: grounded = %+v, want present, Flipped=true", full)
	}
}

// TestCompareCriteriaEnvironmentOnlyNeverFlips: replay can never rebuild
// review_posted/checks_pass - always informational, judge ran or not.
func TestCompareCriteriaEnvironmentOnlyNeverFlips(t *testing.T) {
	res := vetting.ReplayRoundResult{Threshold: 0.7}
	recorded := map[string]float64{"review_posted": 1.0, "checks_pass": 1.0}
	for _, judgeRan := range []bool{false, true} {
		out := compareCriteria(res, recorded, judgeRan)
		for _, c := range out {
			if c.Flipped {
				t.Errorf("judgeRan=%v: %s flipped, want environment-only never to flip: %+v", judgeRan, c.Name, c)
			}
			if !c.HasRecorded || c.HasReplayed {
				t.Errorf("judgeRan=%v: %s = %+v, want HasRecorded=true HasReplayed=false", judgeRan, c.Name, c)
			}
		}
	}
}

// TestCompareCriteriaAddedFailingCriterionFlips: an added-and-failing
// criterion is its own flip class; an added-and-passing one is not.
func TestCompareCriteriaAddedFailingCriterionFlips(t *testing.T) {
	res := vetting.ReplayRoundResult{Threshold: 0.7, Criteria: []vetting.ReplayCriterion{
		{Name: "new_check", Score: 0.2, Reason: "failed"},
		{Name: "new_check_passing", Score: 0.9, Reason: "ok"},
	}}
	out := compareCriteria(res, map[string]float64{}, true)
	byName := map[string]ReplayCriterionReport{}
	for _, c := range out {
		byName[c.Name] = c
	}
	if !byName["new_check"].Flipped {
		t.Errorf("new_check (added, fails) = %+v, want Flipped=true", byName["new_check"])
	}
	if byName["new_check_passing"].Flipped {
		t.Errorf("new_check_passing (added, passes) = %+v, want Flipped=false", byName["new_check_passing"])
	}
}

// TestRunJudgeReplayDeterministicOnlyOmitsJudgeScoredCriteriaWithoutFlipping
// proves it exits clean when the code-owned criteria reproduce, judge-scored ones recorded too.
func TestRunJudgeReplayDeterministicOnlyOmitsJudgeScoredCriteriaWithoutFlipping(t *testing.T) {
	ctx := context.Background()
	bundlePath := buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0, "grounded": 1.0, "answers_question": 1.0})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	opts := ReplayOptions{RubricPath: rubricFile(t, "existence check", 0.85), DeterministicOnly: true}
	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, false, &buf, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (judge-scored criteria absent by design under --deterministic-only): %s", code, buf.String())
	}
	reports := decodeReports(t, &buf)
	names := map[string]bool{}
	for _, c := range reports[0].Criteria {
		names[c.Name] = true
		if c.Name != "cites_sources" && c.Flipped {
			t.Errorf("%s flipped under --deterministic-only, want informational only: %+v", c.Name, c)
		}
	}
	if !names["grounded"] || !names["answers_question"] {
		t.Errorf("criteria = %+v, want the recorded judge-scored criteria still listed (informational)", reports[0].Criteria)
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
	RunJudgeReplay(ctx, fixtureConfig(), sess, opts, judge, nil, false, &buf, false)
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
	RunJudgeReplay(ctx, fixtureConfig(), sess, ReplayOptions{Node: "node-1", RubricPath: rubricPath, DeterministicOnly: true}, nil, nil, false, &buf, true)
	if reports := decodeReports(t, &buf); len(reports) != 1 {
		t.Fatalf("--node node-1: %d report(s), want 1: %s", len(reports), buf.String())
	}

	buf.Reset()
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, ReplayOptions{Node: "no-such-node", RubricPath: rubricPath, DeterministicOnly: true}, nil, nil, false, &buf, true)
	if reports := decodeReports(t, &buf); len(reports) != 0 || code != 1 {
		t.Fatalf("--node no-such-node: %d report(s) exit %d, want 0 reports and exit 1 (an empty filter is not a clean run): %s", len(reports), code, buf.String())
	}

	buf.Reset()
	RunJudgeReplay(ctx, fixtureConfig(), sess, ReplayOptions{Round: 2, RubricPath: rubricPath, DeterministicOnly: true}, nil, nil, false, &buf, true)
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
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, ReplayOptions{DeterministicOnly: true}, nil, nil, false, &buf, false)
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
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, false, &buf, true)
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
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, nil, nil, false, &buf, true)
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

// oneShotJudgeVerdict submits criteria via submit_verdict on the first turn.
type oneShotJudgeVerdict struct{ criteria map[string]any }

func (oneShotJudgeVerdict) Name() string { return "one-shot-judge" }

func (m oneShotJudgeVerdict) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				Name: "submit_verdict", Args: map[string]any{"score": 3, "feedback": "", "criteria": m.criteria},
			}}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

// TestRunJudgeReplayEnvironmentOnlyCriterionDoesNotFlipWhenJudgeRan drives a
// real judge round and proves review_posted stays informational, not a flip.
func TestRunJudgeReplayEnvironmentOnlyCriterionDoesNotFlipWhenJudgeRan(t *testing.T) {
	ctx := context.Background()
	bundlePath := buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0, "review_posted": 1.0})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	judge := vetting.NewJudgeFactory(oneShotJudgeVerdict{criteria: map[string]any{"answers_question": map[string]any{"score": 3}}}, nil, nil)
	opts := ReplayOptions{RubricPath: rubricFile(t, "existence check", 0.85)}
	var buf bytes.Buffer
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, judge, nil, false, &buf, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (review_posted is environment-only, informational): %s", code, buf.String())
	}
	reports := decodeReports(t, &buf)
	var found *ReplayCriterionReport
	for i, c := range reports[0].Criteria {
		if c.Name == "review_posted" {
			found = &reports[0].Criteria[i]
		}
	}
	if found == nil {
		t.Fatalf("criteria = %+v, want a review_posted entry", reports[0].Criteria)
	}
	if found.Flipped {
		t.Errorf("review_posted = %+v, want Flipped=false", found)
	}
}

// artifactProbingJudge calls list_artifacts once, then submits a verdict
// from whatever it got back - proves a stub tool is actually reachable.
type artifactProbingJudge struct {
	t       *testing.T
	turn    int
	gotResp string
}

func (j *artifactProbingJudge) Name() string { return "artifact-probing-judge" }

func (j *artifactProbingJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	j.turn++
	return func(yield func(*model.LLMResponse, error) bool) {
		if j.turn == 1 {
			yield(&model.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "1", Name: "list_artifacts", Args: map[string]any{}}}}},
				FinishReason: genai.FinishReasonStop, TurnComplete: true,
			}, nil)
			return
		}
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "list_artifacts" {
					if s, ok := p.FunctionResponse.Response["result"].(string); ok {
						j.gotResp = s
					}
				}
			}
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "submit_verdict", Args: map[string]any{"score": 3, "feedback": ""}}}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

// TestRunJudgeReplayLocalBundleJudgeCallsArtifactStub proves a local-bundle
// judge round has a real list_artifacts tool to call, not an unknown-tool error.
func TestRunJudgeReplayLocalBundleJudgeCallsArtifactStub(t *testing.T) {
	ctx := context.Background()
	bundlePath := buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0})
	sess, err := bundle.Load(bundlePath)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	probe := &artifactProbingJudge{t: t}
	judge := vetting.NewJudgeFactory(probe, nil, nil)
	stub, err := StubArtifactTools()
	if err != nil {
		t.Fatalf("StubArtifactTools: %v", err)
	}
	opts := ReplayOptions{RubricPath: rubricFile(t, "existence check", 0.85)}
	var buf bytes.Buffer
	// hasRealArtifactAccess=false: the local-bundle path this test exercises.
	code := RunJudgeReplay(ctx, fixtureConfig(), sess, opts, judge, stub, false, &buf, true)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0: %s", code, buf.String())
	}
	if probe.turn < 2 {
		t.Fatalf("judge model got %d turn(s), want >= 2 (list_artifacts then submit_verdict)", probe.turn)
	}
	if !strings.Contains(probe.gotResp, "no artifacts are available") {
		t.Errorf("list_artifacts response = %q, want the stub's message", probe.gotResp)
	}
}

// TestRecordedForKeepsOnlyTheLatestRun proves a queued re-run recording two
// distinct judge-r1 verdicts under the same round label doesn't mix their criteria.
func TestRecordedForKeepsOnlyTheLatestRun(t *testing.T) {
	// Live emits every run's scores under ResponseID = the round label, so only
	// timestamps around the second run's judge turn can tell the runs apart.
	now := time.Now()
	entries := []ledger.Entry{
		{Seq: 1, ChatID: "chat-1", NodeID: "node-1", Round: "judge-r1", Kind: ledger.KindEvalScore, At: now,
			Payload: mustPayload(t, ledger.EvalScorePayload{ResponseID: "judge-r1", Criterion: "cites_sources", Score: 0.2})},
		{Seq: 2, ChatID: "chat-1", NodeID: "node-1", Round: "judge-r1", Kind: ledger.KindEvalScore, At: now.Add(time.Second),
			Payload: mustPayload(t, ledger.EvalScorePayload{ResponseID: "judge-r1", Criterion: "grounded", Score: 0.2})},
		{Seq: 3, ChatID: "chat-1", NodeID: "node-1", Agent: "judge", Round: "judge-r1", Kind: ledger.KindLLMCall, At: now.Add(30 * time.Second),
			Payload: mustPayload(t, ledger.LLMCallPayload{Input: "judge the re-run", Output: "verdict"})},
		{Seq: 4, ChatID: "chat-1", NodeID: "node-1", Round: "judge-r1", Kind: ledger.KindEvalScore, At: now.Add(time.Minute),
			Payload: mustPayload(t, ledger.EvalScorePayload{ResponseID: "judge-r1", Criterion: "cites_sources", Score: 0.9})},
	}
	sess, err := bundle.Load(writeEntries(t, entries))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	rec := recordedFor(sess, judgedRound{node: "node-1", judgeRound: "judge-r1"})
	if len(rec) != 1 || rec["cites_sources"] != 0.9 {
		t.Fatalf("recorded = %+v, want only the re-run's cites_sources=0.9 (the first run's two scores precede its judge turn)", rec)
	}
}

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

const goldenSeedDir = "../../testdata/judge-golden"

// The checked-in set grades clean offline: every deterministic label is actually scored (not passed for
// being absent) and agrees, and the live-model items are skipped, not errored.
func TestScoreGoldenSeedSetAgreesOffline(t *testing.T) {
	cfg, items, err := LoadGoldenSet(goldenSeedDir)
	if err != nil {
		t.Fatal(err)
	}
	rep, code := ScoreGolden(context.Background(), cfg, items, vetting.Config{}, nil, nil)
	if code != 0 {
		t.Fatalf("exit = %d, report: %+v", code, rep)
	}
	skipped := 0
	for _, it := range rep.Items {
		if it.Skipped != "" {
			skipped++
		}
		for _, c := range it.Cells {
			if !c.Scored || c.Outcome != "agree" {
				t.Errorf("%s/%s: scored=%t outcome=%s, want a scored agreement", it.ID, c.Criterion, c.Scored, c.Outcome)
			}
		}
	}
	if skipped != 3 || rep.Overall.Labeled < 8 || rep.Overall.LabelFail == 0 || rep.Overall.LabelPass == 0 {
		t.Errorf("skipped %d, overall %+v; want 3 live items skipped and both pass and fail labels scored", skipped, rep.Overall)
	}
}

func writeGoldenSet(t *testing.T, cfg GoldenConfig, items map[string]GoldenItem) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config.json", cfg)
	for name, it := range items {
		write(name+".json", it)
	}
	return dir
}

func webResearcherRubric(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../agents/web-researcher/rubric.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Mislabeled items surface as one false pass and one false fail; a zero limit fails the run with exit 2.
func TestScoreGoldenCountsBothErrorKindsAndGatesOnLimits(t *testing.T) {
	rubric := webResearcherRubric(t)
	fetched := GoldenActivity{Fetched: []string{"https://example.com/a"}}
	dir := writeGoldenSet(t, GoldenConfig{Threshold: 0.6}, map[string]GoldenItem{
		"backed": {Rubric: rubric, Task: "q", Answer: "See [a](https://example.com/a).", Activity: fetched,
			Labels: map[string]GoldenLabel{"cites_sources": {Pass: false}}},
		"invented": {Rubric: rubric, Task: "q", Answer: "See [b](https://elsewhere.example.org/b).", Activity: fetched,
			Labels: map[string]GoldenLabel{"cites_sources": {Pass: true}}},
	})
	cfg, items, err := LoadGoldenSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	rep, code := ScoreGolden(context.Background(), cfg, items, vetting.Config{}, nil, nil)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (limits exceeded)", code)
	}
	want := GoldenRates{Labeled: 2, LabelFail: 1, LabelPass: 1, FalsePass: 1, FalseFail: 1, FalsePassRate: 1, FalseFailRate: 1}
	if rep.Overall != want {
		t.Errorf("overall = %+v, want %+v", rep.Overall, want)
	}
	if got := strings.Join(rep.Exceeded, "; "); !strings.Contains(got, "cites_sources false-pass rate 1.00 > 0.00") || !strings.Contains(got, "overall false-fail rate") {
		t.Errorf("exceeded = %q, want per-criterion and overall entries", got)
	}
	cfg.MaxFalsePassRate, cfg.MaxFalseFailRate = 1, 1
	if _, code := ScoreGolden(context.Background(), cfg, items, vetting.Config{}, nil, nil); code != 0 {
		t.Errorf("exit = %d with limits at 1, want 0", code)
	}
}

// Only items marked needs_live_model reach the judge, and its raw 0-3 score is read on the rubric scale.
func TestScoreGoldenSendsOnlyLiveItemsToTheJudge(t *testing.T) {
	rubric := webResearcherRubric(t)
	dir := writeGoldenSet(t, GoldenConfig{Threshold: 0.6}, map[string]GoldenItem{
		"live": {Rubric: rubric, NeedsLiveModel: true, Task: "What is the capital of Australia?", Answer: "Canberra.",
			Labels: map[string]GoldenLabel{"answers_question": {Pass: true}}},
		"offline": {Rubric: rubric, Task: "q", Answer: "",
			Labels: map[string]GoldenLabel{"sufficient_length": {Pass: false}}},
	})
	cfg, items, err := LoadGoldenSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	judge := vetting.NewJudgeFactory(sequenceJudgeVerdict{scores: []int{1}, calls: &calls}, nil, nil)
	rep, code := ScoreGolden(context.Background(), cfg, items, vetting.Config{}, judge, nil)
	if calls != 1 {
		t.Errorf("judge calls = %d, want 1 (the live item only)", calls)
	}
	if code != 2 || rep.Overall.FalseFail != 1 || !rep.Live {
		t.Errorf("exit %d overall %+v live %t, want answers_question 1/3 as a false fail", code, rep.Overall, rep.Live)
	}
}

func TestLoadGoldenSetRejectsLabelsOfflineScoringWouldMisread(t *testing.T) {
	rubric := webResearcherRubric(t)
	for label, want := range map[string]string{"answers_question": "set needs_live_model", "checks_pass": "needs a live workspace"} {
		dir := writeGoldenSet(t, GoldenConfig{Threshold: 0.6}, map[string]GoldenItem{
			"x": {Rubric: rubric, Answer: "a", Labels: map[string]GoldenLabel{label: {Pass: true}}},
		})
		if _, _, err := LoadGoldenSet(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("label %s: err = %v, want %q", label, err, want)
		}
	}
	// With no threshold every score would pass, so a set must name its own.
	dir := writeGoldenSet(t, GoldenConfig{}, map[string]GoldenItem{
		"x": {Rubric: rubric, Answer: "a", Labels: map[string]GoldenLabel{"cites_sources": {Pass: true}}},
	})
	if _, _, err := LoadGoldenSet(dir); err == nil || !strings.Contains(err.Error(), "threshold") {
		t.Errorf("err = %v, want a threshold error", err)
	}
}

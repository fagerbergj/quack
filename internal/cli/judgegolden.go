package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/vetting"
)

// GoldenConfig is a golden set's config.json: the gate threshold its labels assume and the error rates
// that fail the run. Rates are fractions of the labels that could be wrong that way (0.1 = 10%).
type GoldenConfig struct {
	Threshold        float64 `json:"threshold"`
	MaxFalsePassRate float64 `json:"max_false_pass_rate"`
	MaxFalseFailRate float64 `json:"max_false_fail_rate"`
}

// GoldenItem is one labeled case: an inline answer (task, answer, activity) or a recorded judge round,
// graded under Rubric (relative to the item file) and checked against human Labels per criterion.
type GoldenItem struct {
	ID               string                 `json:"id"`
	Note             string                 `json:"note,omitempty"`
	Rubric           string                 `json:"rubric"`
	NeedsLiveModel   bool                   `json:"needs_live_model,omitempty"`
	RequireRetrieval bool                   `json:"require_retrieval,omitempty"`
	Task             string                 `json:"task,omitempty"`
	Answer           string                 `json:"answer,omitempty"`
	Activity         GoldenActivity         `json:"activity,omitempty"`
	Recording        *GoldenRecording       `json:"recording,omitempty"`
	Labels           map[string]GoldenLabel `json:"labels"`
	dir              string
}

// path resolves p against the item file's directory unless it is absolute.
func (it GoldenItem) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(it.dir, p)
}

// GoldenActivity is the worker activity an inline item's judge sees: the URLs it fetched.
type GoldenActivity struct {
	Fetched []string `json:"fetched,omitempty"`
}

// GoldenRecording points at one judged round of a recording (a `quack ledger export` bundle or its
// entries.jsonl), relative to the item file.
type GoldenRecording struct {
	Bundle string `json:"bundle"`
	Node   string `json:"node"`
	Round  int    `json:"round"`
}

// GoldenLabel is a human's verdict on one criterion and why.
type GoldenLabel struct {
	Pass   bool   `json:"pass"`
	Reason string `json:"reason"`
}

// GoldenReport is `quack judge score-golden`'s output.
type GoldenReport struct {
	Live     bool               `json:"live"`
	Config   GoldenConfig       `json:"config"`
	Items    []GoldenItemResult `json:"items"`
	Criteria []GoldenRates      `json:"criteria"`
	Overall  GoldenRates        `json:"overall"`
	Exceeded []string           `json:"exceeded,omitempty"`
}

// GoldenItemResult is one item's per-label outcome; Skipped or Error explain an item with no cells.
type GoldenItemResult struct {
	ID      string       `json:"id"`
	Skipped string       `json:"skipped,omitempty"`
	Error   string       `json:"error,omitempty"`
	Cells   []GoldenCell `json:"cells,omitempty"`
}

// GoldenCell compares one label with the gate's call. A criterion the round did not produce cannot fail it
// (weakest-link gates only on present criteria), so it is predicted a pass with Scored false.
type GoldenCell struct {
	Criterion string  `json:"criterion"`
	Label     bool    `json:"label_pass"`
	Predicted bool    `json:"predicted_pass"`
	Scored    bool    `json:"scored"`
	Score     float64 `json:"score"`
	Outcome   string  `json:"outcome"` // agree | false_pass | false_fail
	Reason    string  `json:"reason,omitempty"`
}

// GoldenRates counts one criterion's (or every criterion's) labels and the gate's two kinds of error.
type GoldenRates struct {
	Criterion     string  `json:"criterion,omitempty"`
	Labeled       int     `json:"labeled"`
	LabelFail     int     `json:"label_fail"`
	LabelPass     int     `json:"label_pass"`
	FalsePass     int     `json:"false_pass"`
	FalseFail     int     `json:"false_fail"`
	FalsePassRate float64 `json:"false_pass_rate"`
	FalseFailRate float64 `json:"false_fail_rate"`
}

// LoadGoldenSet reads dir/config.json and every other dir/*.json as an item.
func LoadGoldenSet(dir string) (GoldenConfig, []GoldenItem, error) {
	var cfg GoldenConfig
	if err := readJSONFile(filepath.Join(dir, "config.json"), &cfg); err != nil {
		return cfg, nil, err
	}
	if cfg.Threshold <= 0 || cfg.Threshold > 1 {
		return cfg, nil, fmt.Errorf("golden set %s: config.json threshold %v must be in (0, 1]", dir, cfg.Threshold)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return cfg, nil, err
	}
	var items []GoldenItem
	for _, p := range paths {
		if filepath.Base(p) == "config.json" {
			continue
		}
		it, err := loadGoldenItem(p)
		if err != nil {
			return cfg, nil, err
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return cfg, nil, fmt.Errorf("golden set %s: no items", dir)
	}
	return cfg, items, nil
}

func readJSONFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// loadGoldenItem rejects an item offline scoring would misread: a judge-scored label without
// needs_live_model, or a label on a criterion no recording can rebuild.
func loadGoldenItem(path string) (GoldenItem, error) {
	var it GoldenItem
	if err := readJSONFile(path, &it); err != nil {
		return it, err
	}
	it.dir = filepath.Dir(path)
	if it.ID == "" {
		it.ID = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	if it.Rubric == "" || len(it.Labels) == 0 {
		return it, fmt.Errorf("%s: an item needs a rubric and at least one label", path)
	}
	for name := range it.Labels {
		if vetting.EnvironmentOnlyCriteria[name] {
			return it, fmt.Errorf("%s: %q needs a live workspace and cannot be scored from an item", path, name)
		}
		if !it.NeedsLiveModel && !vetting.IsCodeOwnedCriterion(name) {
			return it, fmt.Errorf("%s: %q is judge-scored; set needs_live_model", path, name)
		}
	}
	return it, nil
}

// ScoreGolden grades every item and returns the report and an exit code: 2 when a rate exceeds the set's
// limit, 1 when an item errored or nothing was scored, else 0. judge nil scores code-owned criteria only.
func ScoreGolden(ctx context.Context, cfg GoldenConfig, items []GoldenItem, base vetting.Config, judge vetting.JudgeFactory, artifactTools []tool.Tool) (GoldenReport, int) {
	rep := GoldenReport{Live: judge != nil, Config: cfg}
	exit := 0
	for _, it := range items {
		res := scoreGoldenItem(ctx, cfg, it, base, judge, artifactTools)
		if res.Error != "" {
			exit = 1
		}
		rep.Items = append(rep.Items, res)
	}
	rep.Criteria, rep.Overall = goldenRates(rep.Items)
	rep.Exceeded = exceededRates(cfg, rep.Criteria, rep.Overall)
	switch {
	case len(rep.Exceeded) > 0:
		exit = 2
	case rep.Overall.Labeled == 0:
		exit = max(exit, 1)
	}
	return rep, exit
}

func scoreGoldenItem(ctx context.Context, cfg GoldenConfig, it GoldenItem, base vetting.Config, judge vetting.JudgeFactory, artifactTools []tool.Tool) GoldenItemResult {
	out := GoldenItemResult{ID: it.ID}
	if it.NeedsLiveModel && judge == nil {
		out.Skipped = "needs a live judge model"
		return out
	}
	gc, rc, err := goldenCase(ctx, cfg, it, base, artifactTools)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	jf := judge
	if !it.NeedsLiveModel {
		jf = nil
	}
	res, err := vetting.ReplayRound(ctx, gc, jf, rc)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	scores := map[string]float64{}
	for _, c := range res.Criteria {
		scores[c.Name] = c.Score
	}
	for _, name := range slices.Sorted(maps.Keys(it.Labels)) {
		out.Cells = append(out.Cells, goldenCell(name, it.Labels[name], scores, cfg.Threshold))
	}
	return out
}

func goldenCell(name string, label GoldenLabel, scores map[string]float64, threshold float64) GoldenCell {
	score, scored := scores[name]
	c := GoldenCell{Criterion: name, Label: label.Pass, Predicted: !scored || score >= threshold, Scored: scored, Score: round4(score), Reason: label.Reason}
	switch {
	case c.Label == c.Predicted:
		c.Outcome = "agree"
	case c.Predicted:
		c.Outcome = "false_pass"
	default:
		c.Outcome = "false_fail"
	}
	return c
}

// goldenCase builds the gate config and replay case for it, from the set's threshold and its own rubric.
func goldenCase(ctx context.Context, cfg GoldenConfig, it GoldenItem, base vetting.Config, artifactTools []tool.Tool) (vetting.Config, vetting.ReplayCase, error) {
	rr, err := vetting.LoadReplayRubric(ctx, nil, "", it.path(it.Rubric))
	if err != nil {
		return base, vetting.ReplayCase{}, err
	}
	gc := base
	gc.Threshold, gc.RequireRetrieval, gc.JudgeArtifactTools = cfg.Threshold, it.RequireRetrieval, artifactTools
	gc.Rubric, gc.RubricSpecs, gc.RubricFixes, gc.RubricPassMarks = rr.Rendered, rr.Specs, rr.Fixes, rr.PassMarks
	if it.Recording != nil {
		rc, err := recordedGoldenCase(it)
		return gc, rc, err
	}
	turns, err := fetchTurns(it.Task, it.Activity.Fetched)
	return gc, vetting.ReplayCase{NodeID: "golden", Task: it.Task, Answer: it.Answer, WorkerTurns: turns}, err
}

// recordedGoldenCase rebuilds the referenced judge round exactly as `quack judge replay` would.
func recordedGoldenCase(it GoldenItem) (vetting.ReplayCase, error) {
	sess, err := bundle.Load(it.path(it.Recording.Bundle))
	if err != nil {
		return vetting.ReplayCase{}, err
	}
	rounds := judgedRounds(sess, ReplayOptions{Node: it.Recording.Node, Round: it.Recording.Round})
	if len(rounds) != 1 {
		return vetting.ReplayCase{}, fmt.Errorf("recording %s: %d judged rounds match node %q round %d, want 1", it.Recording.Bundle, len(rounds), it.Recording.Node, it.Recording.Round)
	}
	task, answer, turns, ok := workerTurnsFor(sess, rounds[0])
	if !ok {
		return vetting.ReplayCase{}, fmt.Errorf("recording %s: no worker round precedes the judge round", it.Recording.Bundle)
	}
	return vetting.ReplayCase{NodeID: rounds[0].node, Task: task, Answer: answer, WorkerTurns: turns, NodeTurns: activityTurnsBefore(sess, rounds[0].at, rounds[0].node)}, nil
}

// fetchTurns renders an inline item's fetches as one recorded worker turn: the task, then a web_fetch
// call and result per URL, the shape the gate's activity scan reads.
func fetchTurns(task string, fetched []string) ([]vetting.RawTurn, error) {
	contents := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: task}}}}
	for i, u := range fetched {
		id := fmt.Sprintf("fetch-%d", i)
		contents = append(contents,
			&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: "web_fetch", Args: map[string]any{"url": u}}}}},
			&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: id, Name: "web_fetch",
				Response: map[string]any{"results": []any{map[string]any{"url": u}}}}}}})
	}
	raw, err := json.Marshal(contents)
	return []vetting.RawTurn{{Input: string(raw)}}, err
}

// goldenRates tallies every scored cell per criterion and overall. A false-pass rate is over the labels
// that say fail, a false-fail rate over the labels that say pass.
func goldenRates(items []GoldenItemResult) ([]GoldenRates, GoldenRates) {
	byName := map[string]*GoldenRates{}
	overall := GoldenRates{}
	for _, it := range items {
		for _, c := range it.Cells {
			r := byName[c.Criterion]
			if r == nil {
				r = &GoldenRates{Criterion: c.Criterion}
				byName[c.Criterion] = r
			}
			r.add(c)
			overall.add(c)
		}
	}
	out := make([]GoldenRates, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, byName[name].finish())
	}
	return out, overall.finish()
}

func (r *GoldenRates) add(c GoldenCell) {
	r.Labeled++
	if c.Label {
		r.LabelPass++
	} else {
		r.LabelFail++
	}
	switch c.Outcome {
	case "false_pass":
		r.FalsePass++
	case "false_fail":
		r.FalseFail++
	}
}

func (r *GoldenRates) finish() GoldenRates {
	if r.LabelFail > 0 {
		r.FalsePassRate = round4(float64(r.FalsePass) / float64(r.LabelFail))
	}
	if r.LabelPass > 0 {
		r.FalseFailRate = round4(float64(r.FalseFail) / float64(r.LabelPass))
	}
	return *r
}

// exceededRates names each rate, per criterion and overall, above the set's limit.
func exceededRates(cfg GoldenConfig, criteria []GoldenRates, overall GoldenRates) []string {
	var out []string
	for _, r := range append(criteria, overall) {
		name := r.Criterion
		if name == "" {
			name = "overall"
		}
		if r.FalsePassRate > cfg.MaxFalsePassRate {
			out = append(out, fmt.Sprintf("%s false-pass rate %.2f > %.2f", name, r.FalsePassRate, cfg.MaxFalsePassRate))
		}
		if r.FalseFailRate > cfg.MaxFalseFailRate {
			out = append(out, fmt.Sprintf("%s false-fail rate %.2f > %.2f", name, r.FalseFailRate, cfg.MaxFalseFailRate))
		}
	}
	return out
}

// RenderGoldenReport prints each disagreement, the per-criterion rates and the verdict.
func RenderGoldenReport(out io.Writer, rep GoldenReport) {
	for _, it := range rep.Items {
		switch {
		case it.Error != "":
			fmt.Fprintf(out, "%s: error: %s\n", it.ID, it.Error)
		case it.Skipped != "":
			fmt.Fprintf(out, "%s: skipped: %s\n", it.ID, it.Skipped)
		}
		for _, c := range it.Cells {
			if c.Outcome != "agree" {
				fmt.Fprintf(out, "%s: %s %s (score %.2f, scored %t; label: %s)\n", it.ID, c.Criterion, c.Outcome, c.Score, c.Scored, c.Reason)
			}
		}
	}
	fmt.Fprintf(out, "%-28s %7s %10s %10s %10s %10s\n", "criterion", "labeled", "label_fail", "false_pass", "label_pass", "false_fail")
	for _, r := range append(rep.Criteria, rep.Overall) {
		name := r.Criterion
		if name == "" {
			name = "overall"
		}
		fmt.Fprintf(out, "%-28s %7d %10d %4d (%3.0f%%) %10d %4d (%3.0f%%)\n", name, r.Labeled, r.LabelFail, r.FalsePass, 100*r.FalsePassRate, r.LabelPass, r.FalseFail, 100*r.FalseFailRate)
	}
	for _, e := range rep.Exceeded {
		fmt.Fprintf(out, "exceeded: %s\n", e)
	}
}

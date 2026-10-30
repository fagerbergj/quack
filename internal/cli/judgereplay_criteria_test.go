package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/vetting"
)

// evidenceModel submits a verdict for answers_question, giving evidence only when its request offers the
// field, as a schema-following judge would, and reports 42 output tokens for the turn.
type evidenceModel struct{ offered *bool }

func (evidenceModel) Name() string { return "evidence-model" }

func (m evidenceModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	tools, _ := json.Marshal(req.Config.Tools)
	*m.offered = strings.Contains(string(tools), `"evidence"`)
	crit := map[string]any{"score": 2}
	if *m.offered {
		crit["evidence"] = "the answer cites one page"
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				Name: "submit_verdict", Args: map[string]any{"score": 2, "feedback": "", "criteria": map[string]any{"answers_question": crit}},
			}}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{CandidatesTokenCount: 42},
		}, nil)
	}
}

func judgedRubric(t *testing.T) string {
	t.Helper()
	doc := `scale: {min: 0, max: 3, pass: 2}
criteria:
  answers_question:
    definition: Addresses the request.
    bands:
    - {min: 2, max: 3, meaning: addresses it}
    - {min: 0, max: 1, meaning: misses it}
`
	path := filepath.Join(t.TempDir(), "rubric.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// replayJudged replays the fixture round with a judge that follows the schema it is shown; offered says
// whether that schema carried the evidence field.
func replayJudged(t *testing.T, cfg *config.Config) (rep ReplayRoundReport, offered bool) {
	t.Helper()
	sess, err := bundle.Load(buildFixtureBundle(t, "worker-r0", map[string]float64{"cites_sources": 1.0}))
	if err != nil {
		t.Fatal(err)
	}
	judge := vetting.NewJudgeFactory(evidenceModel{&offered}, nil, nil)
	var buf bytes.Buffer
	RunJudgeReplay(context.Background(), cfg, sess, ReplayOptions{RubricPath: judgedRubric(t)}, judge, nil, false, &buf, true)
	return decodeReports(t, &buf)[0], offered
}

func criterionNamed(t *testing.T, rep ReplayRoundReport, name string) ReplayCriterionReport {
	t.Helper()
	for _, c := range rep.Criteria {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s criterion in %+v (skipped: %q)", name, rep.Criteria, rep.Skipped)
	return ReplayCriterionReport{}
}

func clefServer(t *testing.T, calls *atomic.Int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"score": map[string]any{"type": "score", "probabilities": map[string]float64{"1": 0.2, "2": 0.8}}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func decisionsFor(url string, enabled bool) config.DecisionsConfig {
	return config.DecisionsConfig{
		Handlers: map[string]config.DecisionHandler{"clef": {URL: url}},
		Points:   map[string]config.DecisionPoint{vetting.JudgeCriteriaPoint: {Enabled: enabled, Handler: "clef", Mode: config.DecisionModeObserve, ActAt: 0.9, Fail: config.DecisionFailOpen}},
	}
}

// TestRunJudgeReplayJudgeCriteriaOnArm: the local quack.yaml enabling judge.criteria makes replay the "on"
// arm, and the report carries what a paired on/off analysis needs. The off arm is the same config without it.
func TestRunJudgeReplayJudgeCriteriaOnArm(t *testing.T) {
	var calls atomic.Int32
	url := clefServer(t, &calls)

	off := fixtureConfig()
	off.Decisions = decisionsFor(url, false)
	offRep, offOffered := replayJudged(t, off)
	oc := criterionNamed(t, offRep, "answers_question")
	if offOffered || oc.Evidence != "" || oc.Point != nil || calls.Load() != 0 {
		t.Errorf("off arm: evidence offered %v, evidence %q, point %+v, clef calls %d; want the judge unchanged and no ask", offOffered, oc.Evidence, oc.Point, calls.Load())
	}
	if offRep.Judge == nil || offRep.Judge.OutputTokens != 42 {
		t.Errorf("off arm judge cost = %+v, want output tokens measured too", offRep.Judge)
	}

	on := fixtureConfig()
	on.Decisions = decisionsFor(url, true)
	onRep, onOffered := replayJudged(t, on)
	c := criterionNamed(t, onRep, "answers_question")
	if !onOffered || c.Evidence != "the answer cites one page" {
		t.Errorf("on arm: evidence offered %v, evidence %q; want the schema field offered and the evidence reported", onOffered, c.Evidence)
	}
	if p := c.Point; p == nil || p.Top != "2" || p.Probabilities["2"] != 0.8 || p.GradeWords {
		t.Errorf("on arm point = %+v, want top 2 with its probabilities and grade_words false", p)
	}
	if c.ReplayedScore < 0.66 || c.ReplayedScore > 0.67 || calls.Load() != 1 {
		t.Errorf("on arm score %v, clef calls %d; want the judge's 2/3 and one ask", c.ReplayedScore, calls.Load())
	}
	if onRep.Judge == nil || onRep.Judge.OutputTokens != 42 || onRep.Judge.Seconds <= 0 {
		t.Errorf("on arm judge cost = %+v, want 42 output tokens and a wall time", onRep.Judge)
	}
}

// replayDecider reads only judge.criteria, so a bad extension point elsewhere cannot fail a replay.
func TestReplayDeciderIgnoresOtherPoints(t *testing.T) {
	d := decisionsFor("http://x", true)
	d.Points["ext:nobody/point"] = config.DecisionPoint{Enabled: true, Handler: "clef", Mode: config.DecisionModeObserve}
	dec, err := replayDecider(d)
	if err != nil || !dec.Enabled(vetting.JudgeCriteriaPoint) {
		t.Fatalf("replayDecider = %v, %v; want judge.criteria enabled despite the unknown extension point", dec, err)
	}
	if dec, err = replayDecider(config.DecisionsConfig{}); dec != nil || err != nil {
		t.Errorf("replayDecider with no decisions = %v, %v; want nil, nil", dec, err)
	}
}

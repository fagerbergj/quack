package vetting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/decide/decidetest"
	"github.com/fagerbergj/quack/internal/ledger"
)

var criteriaSpecs = map[string]criterionSpec{
	"grounded": {Name: "grounded", Definition: " Claims are backed. ", Scale: &scaleSpec{Max: 3},
		Bands: []bandSpec{{Min: 0, Max: 1, Meaning: "unbacked"}, {Min: 2, Max: 3, Meaning: "backed"}}},
	"complete": {Name: "complete", Definition: "Nothing is missing.", Scale: &scaleSpec{Max: 3}, Bands: []bandSpec{{Min: 0, Max: 3, Meaning: "any"}}},
	"owned":    {Name: "owned", Definition: "Code owns it.", Scale: &scaleSpec{Max: 3}, Deterministic: true, Bands: []bandSpec{{Min: 0, Max: 3, Meaning: "any"}}},
	"binary":   {Name: "binary", Definition: "A 0-1 criterion.", Scale: &scaleSpec{Max: 1}, Bands: []bandSpec{{Min: 0, Max: 1, Meaning: "any"}}},
}

func criteriaVerdict() verdict {
	return verdict{Criteria: map[string]criterionScore{
		"grounded": {Score: 2.0 / 3, Checked: "read a.go:10, matches the claim", Shortfall: "SHORTFALL-TEXT", Fix: "FIX-TEXT"},
		"complete": {Score: 1, Checked: "all three asks answered"},
		"owned":    {Score: 1, Checked: "ignored, code-owned", Deterministic: true},
		"binary":   {Score: 1, Checked: "ignored, 0-1 scale"},
		"noproof":  {Score: 1, Checked: "ignored, no rubric spec"},
	}, Feedback: "FEEDBACK-TEXT", Passed: true, Score: 0.66}
}

func criteriaRounds(d *decide.Decider) *judgeRounds {
	return &judgeRounds{cfg: Config{Agent: "web-researcher", Task: "Find the capital.", RubricSpecs: criteriaSpecs, Decisions: d}}
}

func TestJudgeCriteriaPointIsObserveOnly(t *testing.T) {
	if len(judgeCriteria.Questions) != 1 || judgeCriteria.Primary != "score" || len(judgeCriteria.Questions["score"].Criteria.([]string)) != judgeScaleMax+1 {
		t.Fatalf("point = %+v, want one score question with levels 0..%d", judgeCriteria.Point, judgeScaleMax)
	}
	cfg := func(mode string) config.DecisionsConfig {
		return config.DecisionsConfig{Handlers: map[string]config.DecisionHandler{"clef": {URL: decidetest.Down}},
			Points: map[string]config.DecisionPoint{"judge.criteria": {Enabled: true, Handler: "clef", Mode: mode, ActAt: 0.9, Fail: config.DecisionFailOpen}}}
	}
	if _, err := decide.New(cfg(config.DecisionModeObserve)); err != nil {
		t.Errorf("observe: %v", err)
	}
	for _, mode := range []string{config.DecisionModeGuard, config.DecisionModeDecide} {
		if _, err := decide.New(cfg(mode)); err == nil {
			t.Errorf("%s: want a boot error, the caller implements observe only", mode)
		}
	}
}

func TestJudgeCriteriaAsksOncePerGradedCriterion(t *testing.T) {
	asks := criteriaRounds(nil).judgeCriteriaAsks(2, criteriaVerdict())
	if len(asks) != 2 {
		t.Fatalf("asks = %d, want grounded and complete only (code-owned, off-scale, spec-less and empty-evidence skipped)", len(asks))
	}
	want := []struct{ name, baseline string }{{"complete", "3"}, {"grounded", "2"}}
	for i, a := range asks {
		st := a.state.State.(judgeCriteriaState)
		if st.Criterion != want[i].name || a.baseline != want[i].baseline {
			t.Errorf("ask %d = %s baseline %s, want %s baseline %s", i, st.Criterion, a.baseline, want[i].name, want[i].baseline)
		}
	}
}

func TestJudgeCriteriaSkipsEmptyEvidence(t *testing.T) {
	v := criteriaVerdict()
	for _, name := range []string{"complete", "grounded"} {
		c := v.Criteria[name]
		c.Checked = "  \n"
		v.Criteria[name] = c
	}
	if asks := criteriaRounds(nil).judgeCriteriaAsks(1, v); len(asks) != 0 {
		t.Errorf("asks = %d, want none when the judge gave no evidence", len(asks))
	}
}

func TestJudgeCriteriaStateHoldsRubricAndEvidenceOnly(t *testing.T) {
	ask := criteriaRounds(nil).judgeCriteriaAsks(1, criteriaVerdict())[1]
	raw, err := json.Marshal(ask.state.State)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]string
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		if !strings.Contains("node_task criterion definition bands evidence", k) {
			t.Errorf("state key %q is outside the rubric and evidence", k)
		}
	}
	for _, leak := range []string{"SHORTFALL-TEXT", "FIX-TEXT", "FEEDBACK-TEXT", "score", "passed"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("state leaks %q: %s", leak, raw)
		}
	}
	if keys["definition"] != "Claims are backed." || !strings.HasPrefix(keys["bands"], "- **2–3** - backed\n- **0–1** - unbacked") || keys["evidence"] != "read a.go:10, matches the claim" {
		t.Errorf("state = %s, want the trimmed definition, bands highest first and the judge's evidence", raw)
	}
}

// scoreServer answers the score question with level 2 and sends each request's state to states.
func scoreServer(t *testing.T, states chan<- json.RawMessage) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ State json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&body)
		states <- body.State
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"score": map[string]any{"type": "score", "probabilities": map[string]float64{"1": 0.2, "2": 0.8}}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestObserveCriteriaRecordsEachCriterionWithMeta(t *testing.T) {
	mem := decidetest.Capture(t)
	states := make(chan json.RawMessage, 4)
	j := criteriaRounds(decidetest.Decider(t, scoreServer(t, states), judgeCriteria.ID))
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-jc", Node: "n1", Agent: "web-researcher"})
	start := time.Now()
	j.observeCriteria(ctx, 2, criteriaVerdict())
	if time.Since(start) > 200*time.Millisecond {
		t.Error("observeCriteria blocked the caller")
	}
	got := decidetest.Records(t, mem, "chat-jc", 2)
	if len(got) != 2 || len(states) != 2 {
		t.Fatalf("records = %d, requests = %d, want 2 each", len(got), len(states))
	}
	byCriterion := map[string]ledger.DecisionPayload{}
	for _, r := range got {
		var m struct {
			Agent, Criterion string
			Round            int
		}
		_ = json.Unmarshal(r.Meta, &m)
		if r.Point != "judge.criteria" || r.Top != "2" || m.Agent != "web-researcher" || m.Round != 2 || strings.Contains(string(r.State), "meta") {
			t.Errorf("record = %+v meta %s, want a judge.criteria answer 2 with agent and round in meta only", r, r.Meta)
		}
		byCriterion[m.Criterion] = r
	}
	if byCriterion["grounded"].Baseline != "2" || byCriterion["complete"].Baseline != "3" {
		t.Errorf("baselines = grounded %q complete %q, want 2 and 3", byCriterion["grounded"].Baseline, byCriterion["complete"].Baseline)
	}
}

func TestObserveCriteriaDisabledMakesNoCall(t *testing.T) {
	states := make(chan json.RawMessage, 4)
	j := criteriaRounds(decidetest.Decider(t, scoreServer(t, states), answerAccept.ID))
	j.observeCriteria(context.Background(), 1, criteriaVerdict())
	time.Sleep(50 * time.Millisecond)
	if len(states) != 0 {
		t.Errorf("requests = %d, want 0 with judge.criteria off", len(states))
	}
}

// TestSubmittedEvidenceSurvivesNormalisation: the judge's "evidence" key reaches the verdict that observeCriteria reads.
func TestSubmittedEvidenceSurvivesNormalisation(t *testing.T) {
	var args verdictArgs
	if err := json.Unmarshal([]byte(`{"score":2,"criteria":{"grounded":{"evidence":"read a.go:10","score":2}}}`), &args); err != nil {
		t.Fatal(err)
	}
	v := aggregateVerdict(normalizeScale(verdict{Score: args.Score, Criteria: args.Criteria}, criteriaSpecs))
	if c := v.Criteria["grounded"]; c.Checked != "read a.go:10" || c.Score < 0.66 || c.Score > 0.67 {
		t.Errorf("criterion = %+v, want its evidence kept and score 2/3", c)
	}
}

package vetting

import (
	"context"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
)

// judgeCriteria observes a System One grade of each judged criterion from the judge's evidence alone;
// the LLM judge's own score still gates.
var judgeCriteria = decide.Register(decide.Point{ID: "judge.criteria", Primary: "score", Replaces: "criterion_score",
	Modes: []string{config.DecisionModeObserve},
	Questions: map[string]decide.Question{"score": {Type: "score", Criteria: judgeLevels(),
		Instructions: "Using only the evidence, which scoring band does the answer reach for this criterion? Pick that band's integer score."}}},
	func(top string) int { n, _ := strconv.Atoi(top); return n })

// judgeLevels labels the score question's options 0..judgeScaleMax, so an option's index is the raw score.
func judgeLevels() []string {
	out := make([]string, judgeScaleMax+1)
	for i := range out {
		out[i] = strconv.Itoa(i)
	}
	return out
}

const (
	judgeCriteriaTaskMax     = 2000
	judgeCriteriaEvidenceMin = 3000
)

// judgeCriteriaState carries the rubric and the judge's facts, never its score, shortfall, fix or verdict.
type judgeCriteriaState struct {
	NodeTask   string `json:"node_task"`
	Criterion  string `json:"criterion"`
	Definition string `json:"definition"`
	Bands      string `json:"bands"`
	Evidence   string `json:"evidence"`
}

type criterionAsk struct {
	state    decide.Annotated
	baseline string
}

// askable reports whether c is a judge-scored criterion on the 0-judgeScaleMax scale with bands and evidence to grade from.
func askable(c criterionScore, spec criterionSpec) bool {
	return !c.Deterministic && !spec.Deterministic && !c.Unscored && len(spec.Bands) > 0 && strings.TrimSpace(c.Checked) != "" &&
		spec.Scale != nil && *spec.Scale == (scaleSpec{Max: judgeScaleMax})
}

// judgeCriteriaAsks builds one ask per askable criterion in name order. A criterion without evidence or a
// rubric spec is skipped and not recorded, so the report's n counts graded criteria only.
func (j *judgeRounds) judgeCriteriaAsks(round int, v verdict) []criterionAsk {
	task := decide.Clip(j.cfg.Task, judgeCriteriaTaskMax)
	var asks []criterionAsk
	for _, name := range slices.Sorted(maps.Keys(v.Criteria)) {
		c, spec := v.Criteria[name], j.cfg.RubricSpecs[name]
		if !askable(c, spec) {
			slog.Debug("judge.criteria skipped criterion", "component", "vetting", "criterion", name, "round", round)
			continue
		}
		st := judgeCriteriaState{NodeTask: task, Criterion: name, Definition: strings.TrimSpace(spec.Definition), Bands: bandsText(spec.Bands)}
		st.Evidence = decide.Clip(strings.TrimSpace(c.Checked), j.cfg.Decisions.FillBytes(judgeCriteria.ID, judgeCriteriaEvidenceMin, st.NodeTask, st.Definition, st.Bands))
		meta := map[string]any{"agent": j.cfg.Agent, "criterion": name, "round": round}
		asks = append(asks, criterionAsk{state: decide.Annotated{State: st, Meta: meta}, baseline: strconv.Itoa(int(math.Round(c.Score * judgeScaleMax)))})
	}
	return asks
}

// observeCriteria asks judge.criteria about each judged criterion after the round's verdict, on its own
// goroutine and through bulkLane, so a one-slot Clef never has more than one of these ahead of plan.accept.
func (j *judgeRounds) observeCriteria(ctx context.Context, round int, v verdict) {
	if !j.cfg.Decisions.Enabled(judgeCriteria.ID) {
		return
	}
	asks, d := j.judgeCriteriaAsks(round, v), j.cfg.Decisions
	go func() {
		for _, a := range asks {
			bulkLane <- struct{}{}
			<-d.Observe(ctx, judgeCriteria.Point, a.state)(a.baseline)
			<-bulkLane
		}
	}()
}

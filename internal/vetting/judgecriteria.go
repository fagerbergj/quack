package vetting

import (
	"context"
	"log/slog"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

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

// gradeWords marks evidence that states a grade, which a grader would echo instead of grading.
var gradeWords = regexp.MustCompile(`(?i)\b(pass(es|ed)?|fail(s|ed)?|meets?|band|scores?|scored|grade[sd]?|rubric)\b`)

// judgeCriteriaBuilds counts state builds, so tests can see the disabled point do no work.
var judgeCriteriaBuilds atomic.Int64

// levelOf is the judge's integer level for a unit score. Floor, not Round: the gate fails a 1.5 (0.5 unit
// is under any threshold that fails 2), so it must not read as the passing level 2.
func levelOf(unit float64) int { return int(math.Floor(unit*judgeScaleMax + 1e-9)) }

// logger is the round's logger, or the default one for a bare judgeRounds.
func (j *judgeRounds) logger() *slog.Logger {
	if j.log != nil {
		return j.log
	}
	return slog.Default()
}

// judgeCriteriaAsks builds one ask per askable criterion in name order. A criterion without evidence or a
// rubric spec, or one the gate dropped from this node's verdict, is skipped and not recorded, so the
// report's n counts graded criteria only.
func (j *judgeRounds) judgeCriteriaAsks(round int, v verdict) []criterionAsk {
	judgeCriteriaBuilds.Add(1)
	task := decide.Clip(j.cfg.Task, judgeCriteriaTaskMax)
	var asks []criterionAsk
	for _, name := range slices.Sorted(maps.Keys(v.Criteria)) {
		c, spec := v.Criteria[name], j.cfg.RubricSpecs[name]
		if !askable(c, spec) || (name == "structured_verdict" && isNonDeliveringSlice(j.cfg)) {
			j.logger().Debug("judge.criteria skipped criterion", "component", "vetting", "criterion", name, "round", round)
			continue
		}
		st := judgeCriteriaState{NodeTask: task, Criterion: name, Definition: strings.TrimSpace(spec.Definition), Bands: bandsText(spec.Bands)}
		st.Evidence = decide.Clip(strings.TrimSpace(c.Checked), j.cfg.Decisions.FillBytes(judgeCriteria.ID, judgeCriteriaEvidenceMin, st.NodeTask, st.Definition, st.Bands))
		meta := map[string]any{"agent": j.cfg.Agent, "criterion": name, "round": round, "grade_words": gradeWords.MatchString(st.Evidence)}
		asks = append(asks, criterionAsk{state: decide.Annotated{State: st, Meta: meta}, baseline: strconv.Itoa(levelOf(c.Score))})
	}
	return asks
}

// observeCriteria asks about each judged criterion on its own goroutine, one at a time through bulkLane so
// plan.accept never waits behind more than one. A round is 5-12 asks, 5-12 s of Clef: size it with research.source.
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

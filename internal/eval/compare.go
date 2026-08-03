package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/fagerbergj/quack/internal/replay"
)

// CriterionComparison is one rubric criterion's recorded-vs-new judge score.
// *OK is false when that side's bundle carried no score for this criterion
// at all (the rubric changed between runs, or a judge-unavailable round -
// see vetting/judge.go's judge-unavailable path, which emits no evaluation
// event); Delta is only meaningful when both are true.
type CriterionComparison struct {
	Name       string  `json:"name"`
	Recorded   float64 `json:"recorded,omitempty"`
	RecordedOK bool    `json:"recorded_ok"`
	New        float64 `json:"new,omitempty"`
	NewOK      bool    `json:"new_ok"`
	Delta      float64 `json:"delta,omitempty"`
}

// Comparison is eval's whole result for one bundle re-run: the swap that was
// made, both runs' final-answer lengths, and the per-criterion score table
// plus each side's weakest-link overall (vetting.aggregateVerdict's own
// aggregation - the lowest criterion, no averaging, no caps).
type Comparison struct {
	Role              string                `json:"role"`
	Model             string                `json:"model"`
	ChangedAgents     []string              `json:"changed_agents"`
	RecordedAnswerLen int                   `json:"recorded_answer_len"`
	NewAnswerLen      int                   `json:"new_answer_len"`
	Criteria          []CriterionComparison `json:"criteria"`
	RecordedOverall   float64               `json:"recorded_overall"`
	NewOverall        float64               `json:"new_overall"`
}

// Build assembles a Comparison from both bundles' raw evaluation events and
// final-answer texts.
func Build(role, model string, changedAgents []string, recordedScores, newScores []replay.EvalScore, recordedAnswer, newAnswer string) Comparison {
	rec := latestPerCriterion(recordedScores)
	neu := latestPerCriterion(newScores)

	names := make(map[string]bool, len(rec)+len(neu))
	for n := range rec {
		names[n] = true
	}
	for n := range neu {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	c := Comparison{
		Role:              role,
		Model:             model,
		ChangedAgents:     changedAgents,
		RecordedAnswerLen: len(recordedAnswer),
		NewAnswerLen:      len(newAnswer),
		RecordedOverall:   weakestLink(rec),
		NewOverall:        weakestLink(neu),
	}
	for _, n := range sorted {
		cc := CriterionComparison{Name: n}
		if r, ok := rec[n]; ok {
			cc.Recorded, cc.RecordedOK = r.Score, true
		}
		if s, ok := neu[n]; ok {
			cc.New, cc.NewOK = s.Score, true
		}
		if cc.RecordedOK && cc.NewOK {
			cc.Delta = cc.New - cc.Recorded
		}
		c.Criteria = append(c.Criteria, cc)
	}
	return c
}

// latestPerCriterion collapses a bundle's raw evaluation.result events into
// one score per criterion name: the LATEST (by timestamp) reading, which is
// the gate's final verdict for whichever node/round most recently judged it
// (an earlier, lower score from a revise loop is superseded - the gate's own
// pass/fail decision only ever looks at the last round, see vetting/node.go).
//
// v1 ceiling: a multi-node run's SAME criterion name from two different
// nodes collapses into one row - fine for the common single/few-node eval
// target this feature ships for, not a cross-node breakdown.
func latestPerCriterion(scores []replay.EvalScore) map[string]replay.EvalScore {
	out := map[string]replay.EvalScore{}
	for _, s := range scores {
		if prev, ok := out[s.Criterion]; !ok || s.Timestamp.After(prev.Timestamp) {
			out[s.Criterion] = s
		}
	}
	return out
}

// weakestLink is vetting.aggregateVerdict's own rule applied here: the
// lowest criterion score, or 0 when there are none.
func weakestLink(m map[string]replay.EvalScore) float64 {
	if len(m) == 0 {
		return 0
	}
	lowest := 1.0
	for _, s := range m {
		if s.Score < lowest {
			lowest = s.Score
		}
	}
	return lowest
}

// Render writes c as a human-readable comparison table.
func Render(w io.Writer, c Comparison) {
	fmt.Fprintf(w, "eval: role=%s model=%s agents=%s\n", c.Role, c.Model, strings.Join(c.ChangedAgents, ","))
	fmt.Fprintf(w, "answer length: recorded=%d new=%d (%+d)\n\n", c.RecordedAnswerLen, c.NewAnswerLen, c.NewAnswerLen-c.RecordedAnswerLen)
	fmt.Fprintf(w, "%-28s %10s %10s %10s\n", "criterion", "recorded", "new", "delta")
	for _, cc := range c.Criteria {
		fmt.Fprintf(w, "%-28s %10s %10s %10s\n", cc.Name, scoreCell(cc.Recorded, cc.RecordedOK), scoreCell(cc.New, cc.NewOK), deltaCell(cc))
	}
	fmt.Fprintf(w, "%-28s %10s %10s %10s\n", "overall (weakest-link)",
		fmt.Sprintf("%.2f", c.RecordedOverall), fmt.Sprintf("%.2f", c.NewOverall),
		fmt.Sprintf("%+.2f", c.NewOverall-c.RecordedOverall))
}

func scoreCell(v float64, ok bool) string {
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", v)
}

func deltaCell(cc CriterionComparison) string {
	if !(cc.RecordedOK && cc.NewOK) {
		return "n/a"
	}
	return fmt.Sprintf("%+.2f", cc.Delta)
}

// RenderJSON writes c as the --json shape.
func RenderJSON(w io.Writer, c Comparison) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(c)
}

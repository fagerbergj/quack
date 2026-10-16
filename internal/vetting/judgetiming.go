package vetting

import (
	"math"
	"time"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// Judge round phases, as the judge_round record and quack.judge.phase.duration name them.
const (
	phasePrepare       = "prepare"       // activity scan, episodic revision, judge prompt resolution
	phaseDeterministic = "deterministic" // computeDeterministicCriteria
	phaseAdmission     = "admission"     // waiting for a judge-model slot
	phaseVerify        = "verify"        // the specifics_supported page check
	phaseJudgeTools    = "judge_tools"   // judge sessions up to their last tool result
	phaseJudgeVerdict  = "judge_verdict" // from the last tool result (or session start) to the verdict
)

// JudgeRoundTiming is where one judge round's wall time went. JudgeCalls counts judge sessions, so a
// transient retry, a no-verdict retry or a re-judge shows as more than one.
type JudgeRoundTiming struct {
	PhaseSeconds map[string]float64 `json:"phase_seconds"`
	JudgeCalls   int                `json:"judge_calls"`
	JudgeTurns   int                `json:"judge_turns"`
	ToolCalls    int                `json:"tool_calls"`
}

func newJudgeRoundTiming() *JudgeRoundTiming {
	return &JudgeRoundTiming{PhaseSeconds: map[string]float64{}}
}

// since adds the time from start to now to phase; nil-safe so replay and tests need no timing.
func (t *JudgeRoundTiming) since(phase string, start time.Time) {
	if t != nil {
		t.PhaseSeconds[phase] += time.Since(start).Seconds()
	}
}

// addJudgeSession splits one judge session at its last tool result: before it is tool turns, after it
// the verdict call (and any nudge or forced close).
func (t *JudgeRoundTiming) addJudgeSession(start time.Time, st *judgeRoundState) {
	if t == nil {
		return
	}
	split := start
	if st.lastToolAt.After(start) {
		split = st.lastToolAt
	}
	t.PhaseSeconds[phaseJudgeTools] += split.Sub(start).Seconds()
	t.since(phaseJudgeVerdict, split)
	t.JudgeCalls++
	t.JudgeTurns += st.turns
	t.ToolCalls += st.toolCalls
}

// addCounts adds o's judge sessions, turns and tool calls to t. o ran concurrently with t, so its
// phase seconds overlap t's wall time and are not added.
func (t *JudgeRoundTiming) addCounts(o *JudgeRoundTiming) {
	if t == nil || o == nil {
		return
	}
	t.JudgeCalls += o.JudgeCalls
	t.JudgeTurns += o.JudgeTurns
	t.ToolCalls += o.ToolCalls
}

// emit records every measured phase as quack.judge.phase.duration; verify is the specifics_supported check.
func (t *JudgeRoundTiming) emit(agent string) {
	if t == nil {
		return
	}
	for phase, s := range t.PhaseSeconds {
		criterion := ""
		if phase == phaseVerify {
			criterion = specificsSupportedCriterion
		}
		otelobs.RecordJudgePhase(agent, phase, criterion, s)
	}
}

// rounded is t with each phase rounded to the millisecond, for the stored record.
func (t *JudgeRoundTiming) rounded() *JudgeRoundTiming {
	if t == nil {
		return nil
	}
	out := *t
	out.PhaseSeconds = make(map[string]float64, len(t.PhaseSeconds))
	for phase, s := range t.PhaseSeconds {
		out.PhaseSeconds[phase] = math.Round(s*1000) / 1000
	}
	return &out
}

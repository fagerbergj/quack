package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/vetting"
)

// ReplayOptions is `quack judge replay`'s filters and mode.
type ReplayOptions struct {
	Node              string // "" = every judged node
	Round             int    // 0 = every judge round; else just judge-r<Round>
	RubricPath        string // "" = the graded node's own bundled rubric.yaml
	DeterministicOnly bool   // skip the judge model entirely
}

// ReplayCriterionReport is one criterion's recorded-vs-replayed comparison.
// Pass/fail is decided by Threshold - the live gate's own bar - never RubricMark.
type ReplayCriterionReport struct {
	Name           string  `json:"name"`
	HasRecorded    bool    `json:"has_recorded"`
	RecordedScore  float64 `json:"recorded_score,omitempty"`
	HasReplayed    bool    `json:"has_replayed"`
	ReplayedScore  float64 `json:"replayed_score,omitempty"`
	RubricMark     float64 `json:"rubric_mark,omitempty"`
	Threshold      float64 `json:"threshold"`
	RecordedPassed bool    `json:"recorded_passed"`
	ReplayedPassed bool    `json:"replayed_passed"`
	Deterministic  bool    `json:"deterministic"`
	Reason         string  `json:"reason,omitempty"`
	Flipped        bool    `json:"flipped"`
}

// ReplayRoundReport is one judged round's comparison.
type ReplayRoundReport struct {
	Node     string                  `json:"node"`
	Agent    string                  `json:"agent"`
	Round    string                  `json:"round"`
	Criteria []ReplayCriterionReport `json:"criteria"`
	Flipped  bool                    `json:"flipped"`
	Skipped  string                  `json:"skipped,omitempty"`
	Note     string                  `json:"note,omitempty"`
	// ArtifactsWritten: this round's activity wrote these artifacts - the
	// recording carries no artifact body, only its ledger pointer.
	ArtifactsWritten []string `json:"artifacts_written,omitempty"`
}

// judgedRound is one (node, judge round) this bundle recorded a verdict for.
type judgedRound struct {
	node, agent, judgeRound string
	at                      time.Time // this judge round's own latest activity
}

// judgedRounds finds every (node, judge round) sess recorded eval.score
// entries for, filtered by opts, oldest node/round first.
func judgedRounds(sess *bundle.Session, opts ReplayOptions) []judgedRound {
	type key struct{ node, round string }
	byKey := map[key]bool{}
	for _, sc := range sess.EvaluationResults() {
		byKey[key{sc.Node, sc.Round}] = true
	}
	agentOf := map[string]string{}
	agentAt := map[string]time.Time{}
	for _, k := range sess.Streams() {
		if k.Node == "" || k.Agent == "" || k.Agent == "judge" {
			continue
		}
		// Streams() is map-ordered; a node with two worker agents resolves to the one that answered last.
		if _, _, at, ok := sess.RoundTaskAnswer(k); ok && !at.Before(agentAt[k.Node]) {
			agentOf[k.Node], agentAt[k.Node] = k.Agent, at
		}
	}
	out := make([]judgedRound, 0, len(byKey))
	for k := range byKey {
		if opts.Node != "" && k.node != opts.Node {
			continue
		}
		if _, ok := judgeRoundNum(k.round); !ok {
			continue
		}
		if opts.Round != 0 {
			if n, _ := judgeRoundNum(k.round); n != opts.Round {
				continue
			}
		}
		out = append(out, judgedRound{node: k.node, agent: agentOf[k.node], judgeRound: k.round, at: judgeRoundTime(sess, k.node, k.round)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].node != out[j].node {
			return out[i].node < out[j].node
		}
		return out[i].at.Before(out[j].at)
	})
	return out
}

// judgeRoundNum parses "judge-rN" into N; false for anything else.
func judgeRoundNum(round string) (int, bool) {
	n, ok := strings.CutPrefix(round, "judge-r")
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(n)
	if err != nil {
		return 0, false
	}
	return i, true
}

// judgeRoundTime is (node, round)'s latest activity - the judge stream's
// last chat turn, or its eval.score entries' latest timestamp (forced close).
func judgeRoundTime(sess *bundle.Session, node, round string) time.Time {
	turns := sess.ChatTurns(bundle.StreamKey{Node: node, Agent: "judge", Round: round})
	if n := len(turns); n > 0 {
		return turns[n-1].At
	}
	var latest time.Time
	for _, sc := range sess.EvaluationResults() {
		if sc.Node == node && sc.Round == round && sc.Timestamp.After(latest) {
			latest = sc.Timestamp
		}
	}
	return latest
}

// nodeRound is one non-judge round's recorded task/answer/time, chronology-matched
// to a judge round rather than assumed from its round-id string (hitl/confirm/cont/revise/-sN all vary).
type nodeRound struct {
	task, answer string
	at           time.Time
}

// nodeRoundsBefore returns node's own non-judge rounds with activity at or
// before judgeAt, oldest first - workerTurnsFor's task/answer source.
func nodeRoundsBefore(sess *bundle.Session, node string, judgeAt time.Time) []nodeRound {
	var out []nodeRound
	for _, k := range sess.Streams() {
		if k.Node != node || k.Agent == "" || k.Agent == "judge" {
			continue
		}
		task, answer, at, ok := sess.RoundTaskAnswer(k)
		if !ok || at.After(judgeAt) {
			continue
		}
		out = append(out, nodeRound{task: task, answer: answer, at: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// activityTurnsBefore gathers every stream's (any node - the live gate scans
// the whole chat session) last llm.call at or before judgeAt, time-ordered.
func activityTurnsBefore(sess *bundle.Session, judgeAt time.Time) []vetting.RawTurn {
	type timedTurn struct {
		turn vetting.RawTurn
		at   time.Time
	}
	var found []timedTurn
	for _, k := range sess.Streams() {
		if k.Agent == "judge" {
			continue
		}
		turns := sess.ChatTurns(k)
		var last *bundle.ChatTurn
		for i := range turns {
			if turns[i].At.After(judgeAt) {
				break
			}
			last = &turns[i]
		}
		if last != nil {
			found = append(found, timedTurn{vetting.RawTurn{Input: last.Input, Output: last.Output}, last.At})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].at.Before(found[j].at) })
	out := make([]vetting.RawTurn, len(found))
	for i, f := range found {
		out[i] = f.turn
	}
	return out
}

// workerTurnsFor rebuilds jr's gate inputs: the node's earliest-round task,
// its latest-before-jr.at answer, and the whole session's activity turns.
func workerTurnsFor(sess *bundle.Session, jr judgedRound) (task, answer string, turns []vetting.RawTurn, ok bool) {
	rounds := nodeRoundsBefore(sess, jr.node, jr.at)
	if len(rounds) == 0 {
		return "", "", nil, false
	}
	return rounds[0].task, rounds[len(rounds)-1].answer, activityTurnsBefore(sess, jr.at), true
}

// recordedFor returns jr's latest run's per-criterion scores. A re-run shares the round label AND the
// ResponseID (live passes the round id), so the split is by time: scores at or after the judge's last turn.
func recordedFor(sess *bundle.Session, jr judgedRound) map[string]float64 {
	var cutoff time.Time
	if turns := sess.ChatTurns(bundle.StreamKey{Node: jr.node, Agent: "judge", Round: jr.judgeRound}); len(turns) > 0 {
		cutoff = turns[len(turns)-1].At
	}
	out := map[string]float64{}
	for _, sc := range sess.EvaluationResults() {
		if sc.Node == jr.node && sc.Round == jr.judgeRound && !sc.Timestamp.Before(cutoff) {
			out[sc.Criterion] = sc.Score
		}
	}
	return out
}

// RubricConfigFor builds cfg's gate Config for one agent bundle: the same
// base+bundle-override rubric resolution serve.go's perAgentGateCfg does.
func RubricConfigFor(ctx context.Context, cfg *config.Config, agentName, rubricOverride string, judgeArtifactTools []tool.Tool) (vetting.Config, error) {
	if agentName == "" {
		return vetting.Config{}, fmt.Errorf("judge replay: no recorded worker agent for this node (an ACP invoke record may be missing from the bundle)")
	}
	base, err := vetting.FromConfig(ctx, nil, cfg.Gates)
	if err != nil {
		return vetting.Config{}, err
	}
	ac, ok := cfg.Agents[agentName]
	if !ok {
		return vetting.Config{}, fmt.Errorf("judge replay: agent %q not found in quack.yaml", agentName)
	}
	rr, err := vetting.LoadReplayRubric(ctx, nil, ac.Bundle, rubricOverride)
	if err != nil {
		return vetting.Config{}, err
	}
	base.Rubric = rr.Rendered
	base.RubricSpecs, base.RubricFixes, base.RubricPassMarks = rr.Specs, rr.Fixes, rr.PassMarks
	base.ReadOnly, base.RequireRetrieval = vetting.AgentToolPolicy(ac.Tools, ac.Acp)
	base.JudgeArtifactTools = judgeArtifactTools
	return base, nil
}

// BuildReplayJudge builds the local quack.yaml's configured judge model as a
// JudgeFactory with no read/skill tools. nil, nil when no judge is configured.
func BuildReplayJudge(cfg *config.Config, newModel func(config.ProviderConfig, string) (model.LLM, error)) (vetting.JudgeFactory, error) {
	if !cfg.Gates.JudgeEnabled() {
		return nil, nil
	}
	jprov, ok := cfg.Provider(cfg.Gates.Judge.Provider)
	if !ok {
		return nil, fmt.Errorf("judge replay: gates.judge: provider %q not found", cfg.Gates.Judge.Provider)
	}
	judge, err := newModel(jprov, cfg.Gates.Judge.Model)
	if err != nil {
		return nil, fmt.Errorf("judge replay: gates.judge: model: %w", err)
	}
	return vetting.NewJudgeFactory(judge, nil, nil), nil
}

// RunJudgeReplay replays opts' judged rounds. hasRealArtifactAccess false (a
// local bundle) replays an artifact-writing round deterministic-only. Exit != 0 on any flip.
func RunJudgeReplay(ctx context.Context, cfg *config.Config, sess *bundle.Session, opts ReplayOptions, judge vetting.JudgeFactory, judgeArtifactTools []tool.Tool, hasRealArtifactAccess bool, out io.Writer, asJSON bool) int {
	rounds := judgedRounds(sess, opts)
	reports := make([]ReplayRoundReport, 0, len(rounds))
	exit := 0
	if len(rounds) == 0 && (opts.Node != "" || opts.Round != 0) {
		// An empty filtered run must not read as a clean one: exit like a skipped round.
		if !asJSON {
			fmt.Fprintln(out, "the --node/--round filter matched no judged round; nothing was replayed")
		}
		exit = 1
	}
	cfgCache := map[string]vetting.Config{}
	for _, jr := range rounds {
		rep, code := replayOneRound(ctx, cfg, sess, jr, opts, judge, judgeArtifactTools, hasRealArtifactAccess, cfgCache)
		exit = maxInt(exit, code)
		reports = append(reports, rep)
	}
	if asJSON {
		_ = WriteJSON(out, reports)
	} else {
		renderReplayReports(out, reports)
	}
	return exit
}

// replayOneRound replays jr, returning its report and an exit code
// contribution (0 clean, 1 skipped/error, 2 a flip).
func replayOneRound(ctx context.Context, cfg *config.Config, sess *bundle.Session, jr judgedRound, opts ReplayOptions, judge vetting.JudgeFactory, judgeArtifactTools []tool.Tool, hasRealArtifactAccess bool, cfgCache map[string]vetting.Config) (ReplayRoundReport, int) {
	rep := ReplayRoundReport{Node: jr.node, Agent: jr.agent, Round: jr.judgeRound}
	task, answer, turns, ok := workerTurnsFor(sess, jr)
	if !ok {
		rep.Skipped = "no recorded worker round precedes this judge round to rebuild its answer from"
		return rep, 1
	}
	gc, ok := cfgCache[jr.agent]
	if !ok {
		var err error
		gc, err = RubricConfigFor(ctx, cfg, jr.agent, opts.RubricPath, judgeArtifactTools)
		if err != nil {
			rep.Skipped = err.Error()
			return rep, 1
		}
		cfgCache[jr.agent] = gc
	}
	rc := vetting.ReplayCase{NodeID: jr.node, Task: task, Answer: answer, WorkerTurns: turns}

	jf := judge
	if opts.DeterministicOnly {
		jf = nil
	}
	if jf != nil && !hasRealArtifactAccess {
		// Cheap pre-check: did this round write an artifact the judge can't read here?
		written, err := vetting.RebuildActivityArtifacts(ctx, gc, rc)
		if err == nil && len(written) > 0 {
			jf = nil
			rep.Note = fmt.Sprintf("wrote artifact(s) %v; no real artifact access (local bundle - use --from-server), replayed deterministic-only", written)
		}
	}
	res, err := vetting.ReplayRound(ctx, gc, jf, rc)
	if err != nil {
		rep.Skipped = err.Error()
		return rep, 1
	}
	rep.ArtifactsWritten = res.ArtifactsWritten
	rep.Criteria = compareCriteria(res, recordedFor(sess, jr), jf != nil)
	for _, c := range rep.Criteria {
		if c.Flipped {
			rep.Flipped = true
		}
	}
	if rep.Flipped {
		return rep, 2
	}
	return rep, 0
}

// compareCriteria decides pass/fail by res.Threshold: an environment-only
// recorded criterion is always informational; an added failing one is its own flip class.
func compareCriteria(res vetting.ReplayRoundResult, recorded map[string]float64, judgeRan bool) []ReplayCriterionReport {
	out := make([]ReplayCriterionReport, 0, len(res.Criteria)+len(recorded))
	seen := map[string]bool{}
	for _, c := range res.Criteria {
		seen[c.Name] = true
		rec, hasRec := recorded[c.Name]
		recPass := hasRec && rec >= res.Threshold
		replPass := c.Score >= res.Threshold
		reason, flipped := c.Reason, recPass != replPass
		if !hasRec {
			flipped = !replPass
			if flipped {
				reason = "added by the working-copy rubric, and fails: " + reason
			}
		}
		out = append(out, ReplayCriterionReport{
			Name: c.Name, HasRecorded: hasRec, RecordedScore: rec, HasReplayed: true, ReplayedScore: c.Score,
			RubricMark: c.RubricMark, Threshold: res.Threshold, RecordedPassed: recPass, ReplayedPassed: replPass,
			Deterministic: c.Deterministic, Reason: reason, Flipped: flipped,
		})
	}
	extra := make([]string, 0)
	for name := range recorded {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		rec := recorded[name]
		reason, flipped := "recorded, not replayed (judge not run this replay)", false
		switch {
		case vetting.EnvironmentOnlyCriteria[name]:
			reason, flipped = "recorded, not replayed (not rebuildable from a recording)", false
		case judgeRan:
			reason, flipped = "recorded, not replayed", true
		}
		out = append(out, ReplayCriterionReport{
			Name: name, HasRecorded: true, RecordedScore: rec, HasReplayed: false, Threshold: res.Threshold,
			RecordedPassed: rec >= res.Threshold, Reason: reason, Flipped: flipped,
		})
	}
	return out
}

func renderReplayReports(out io.Writer, reports []ReplayRoundReport) {
	for _, r := range reports {
		fmt.Fprintf(out, "%s %s\n", r.Node, r.Round)
		if r.Skipped != "" {
			fmt.Fprintf(out, "  skipped: %s\n", r.Skipped)
			continue
		}
		if r.Note != "" {
			fmt.Fprintf(out, "  note: %s\n", r.Note)
		}
		for _, c := range r.Criteria {
			flag := " "
			if c.Flipped {
				flag = "!"
			}
			fmt.Fprintf(out, "  %s %-24s recorded=%-12s replayed=%-12s rubric_mark=%.2f threshold=%.2f  %s\n",
				flag, c.Name, scoreCell(c.HasRecorded, c.RecordedScore, c.RecordedPassed),
				scoreCell(c.HasReplayed, c.ReplayedScore, c.ReplayedPassed), c.RubricMark, c.Threshold, c.Reason)
		}
	}
}

// scoreCell renders one recorded/replayed cell: "-" when absent, else the
// score with its pass/fail word.
func scoreCell(has bool, score float64, passed bool) string {
	if !has {
		return "-"
	}
	return fmt.Sprintf("%.2f(%s)", score, passWord(passed))
}

func passWord(passed bool) string {
	if passed {
		return "pass"
	}
	return "fail"
}

func maxInt(a, b int) int {
	if b > a {
		return b
	}
	return a
}

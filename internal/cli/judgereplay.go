package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/vetting"
)

// ReplayOptions is `quack judge replay`'s filters and mode.
type ReplayOptions struct {
	Node              string             // "" = every judged node
	Round             int                // 0 = every judge round; else just judge-r<Round>
	RubricPath        string             // "" = the graded node's own bundled rubric.yaml
	DeterministicOnly bool               // skip the judge model entirely
	Repeat            int                // judge each round this many times and report the spread (1 = once)
	Pages             vetting.PageLoader // stored web pages for the shadow locate tier; nil skips it
	Verifier          *vetting.Verifier  // the shadow verify tier over located specifics; nil skips it
	ReportPath        string             // also write the JSON report here; "" = stdout only
}

// ReplayReport is `quack judge replay --json`'s whole output: sorted, rounded and timestamp-free so two
// runs over one recording diff cleanly. Consistency is set only when --repeat measured something.
type ReplayReport struct {
	Repeat      int                 `json:"repeat"`
	Consistency *ReplayConsistency  `json:"consistency,omitempty"`
	Rounds      []ReplayRoundReport `json:"rounds"`
}

// ReplayConsistency summarises every --repeat measurement in the report. Agreement is the fraction of
// repeats that agree with the majority pass/fail: 1 is a judge that never flips, 0.5 a coin toss.
type ReplayConsistency struct {
	Rounds             int     `json:"rounds"`              // rounds judged repeatedly
	Criteria           int     `json:"criteria"`            // (round, judge-scored criterion) cells measured
	Unstable           int     `json:"unstable"`            // cells whose repeats disagreed on pass/fail
	CriterionAgreement float64 `json:"criterion_agreement"` // mean agreement over the cells
	RoundAgreement     float64 `json:"round_agreement"`     // mean agreement on the whole round's pass/fail
	MaxSD              float64 `json:"max_sd"`
}

// ReplaySpread is one criterion's (or, without Samples, one round's) scores across --repeat passes.
type ReplaySpread struct {
	N         int       `json:"n"`
	Samples   []float64 `json:"samples,omitempty"`
	Mean      float64   `json:"mean"`
	SD        float64   `json:"sd"`
	Min       float64   `json:"min"`
	Max       float64   `json:"max"`
	PassRate  float64   `json:"pass_rate"`
	Agreement float64   `json:"agreement"`
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
	// Spread: this judge-scored criterion's replayed scores across --repeat passes.
	Spread *ReplaySpread `json:"spread,omitempty"`
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
	// Units: the shadow locate tier's rows, one per specific; only with stored page access.
	Units []UnitRow `json:"units,omitempty"`
	// Spread: the whole round's pass/fail (every criterion at or above threshold) across --repeat passes.
	Spread *ReplaySpread `json:"spread,omitempty"`
}

// UnitRow is one specific's locate result, as the design's failed-round table shows it.
type UnitRow struct {
	Kind     string `json:"kind"`
	Unit     string `json:"unit"`
	Specific string `json:"specific"`
	Citation string `json:"citation,omitempty"`
	State    string `json:"state"`
	Window   string `json:"window,omitempty"`
	Verify   string `json:"verify,omitempty"` // the verify tier's state, when it ran
	Quote    string `json:"quote,omitempty"`
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
// the whole chat session) last llm.call at or before judgeAt, time-ordered; node narrows it to one node's streams.
func activityTurnsBefore(sess *bundle.Session, judgeAt time.Time, node string) []vetting.RawTurn {
	type timedTurn struct {
		turn vetting.RawTurn
		at   time.Time
	}
	var found []timedTurn
	for _, k := range sess.Streams() {
		if k.Agent == "judge" || (node != "" && k.Node != node) {
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
	return rounds[0].task, rounds[len(rounds)-1].answer, activityTurnsBefore(sess, jr.at, ""), true
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

// rubricConfigFor builds cfg's gate Config for one agent bundle: the same
// base+bundle-override rubric resolution serve.go's perAgentGateCfg does.
func rubricConfigFor(ctx context.Context, cfg *config.Config, agentName, rubricOverride string, judgeArtifactTools []tool.Tool) (vetting.Config, error) {
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

// BuildReplayVerifier builds the configured judge model as the tool-less verify
// tier; nil, nil when no judge is configured.
func BuildReplayVerifier(cfg *config.Config, newModel func(config.ProviderConfig, string) (model.LLM, error)) (*vetting.Verifier, error) {
	if !cfg.Gates.JudgeEnabled() {
		return nil, nil
	}
	jprov, ok := cfg.Provider(cfg.Gates.Judge.Provider)
	if !ok {
		return nil, fmt.Errorf("judge replay: gates.judge: provider %q not found", cfg.Gates.Judge.Provider)
	}
	llm, err := newModel(jprov, cfg.Gates.Judge.Model)
	if err != nil {
		return nil, fmt.Errorf("judge replay: verifier model: %w", err)
	}
	return &vetting.Verifier{LLM: llm}, nil
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
	for i, jr := range rounds {
		rep, code := replayOneRound(ctx, cfg, sess, jr, opts, judge, judgeArtifactTools, hasRealArtifactAccess, cfgCache)
		exit = max(exit, code)
		reports = append(reports, rep)
		// stderr progress: a --repeat run over many rounds is otherwise silent until the end
		fmt.Fprintf(os.Stderr, "replayed %d/%d %s %s exit=%d\n", i+1, len(rounds), rep.Node, rep.Round, code)
	}
	report := ReplayReport{Repeat: max(opts.Repeat, 1), Consistency: consistencyOf(reports), Rounds: reports}
	if asJSON {
		_ = WriteJSON(out, report)
	} else {
		renderReplayReports(out, report)
	}
	if opts.ReportPath != "" {
		if err := WriteJSONFile(opts.ReportPath, report); err != nil {
			fmt.Fprintf(os.Stderr, "judge replay: %v\n", err)
			exit = max(exit, 1)
		}
	}
	return exit
}

// WriteJSONFile writes v as indented JSON to path, the same bytes --json prints.
func WriteJSONFile(path string, v any) error {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// consistencyOf folds every round's --repeat spreads into one summary; nil when nothing was repeated.
func consistencyOf(reports []ReplayRoundReport) *ReplayConsistency {
	var c ReplayConsistency
	var critSum, roundSum float64
	for _, r := range reports {
		if r.Spread == nil {
			continue
		}
		c.Rounds++
		roundSum += r.Spread.Agreement
		for _, cr := range r.Criteria {
			if cr.Spread == nil {
				continue
			}
			c.Criteria++
			critSum += cr.Spread.Agreement
			c.MaxSD = max(c.MaxSD, cr.Spread.SD)
			if cr.Spread.Agreement < 1 {
				c.Unstable++
			}
		}
	}
	if c.Rounds == 0 {
		return nil
	}
	c.RoundAgreement = round4(roundSum / float64(c.Rounds))
	if c.Criteria > 0 {
		c.CriterionAgreement = round4(critSum / float64(c.Criteria))
	}
	return &c
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
		gc, err = rubricConfigFor(ctx, cfg, jr.agent, opts.RubricPath, judgeArtifactTools)
		if err != nil {
			rep.Skipped = err.Error()
			return rep, 1
		}
		cfgCache[jr.agent] = gc
	}
	rc := vetting.ReplayCase{NodeID: jr.node, Task: task, Answer: answer, WorkerTurns: turns, NodeTurns: activityTurnsBefore(sess, jr.at, jr.node), Pages: opts.Pages, Verifier: opts.Verifier}

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
	rep.Units = unitRows(res.Units)
	rep.Criteria = compareCriteria(res, recordedFor(sess, jr), jf != nil)
	var incomplete bool
	rep.Spread, rep.Note, incomplete = noteRepeatSpread(ctx, gc, jf, rc, res, opts.Repeat, rep.Criteria, rep.Note)
	for _, c := range rep.Criteria {
		if c.Flipped {
			rep.Flipped = true
		}
	}
	if rep.Flipped {
		return rep, 2
	}
	if incomplete {
		return rep, 1 // a requested measurement that lost a pass is not a clean run
	}
	return rep, 0
}

// noteRepeatSpread runs the --repeat measurement when it applies and folds a failed pass into the
// round note, keeping replayOneRound's own branching flat.
func noteRepeatSpread(ctx context.Context, gc vetting.Config, jf vetting.JudgeFactory, rc vetting.ReplayCase, res vetting.ReplayRoundResult, repeat int, crit []ReplayCriterionReport, note string) (*ReplaySpread, string, bool) {
	if jf == nil || repeat < 2 {
		return nil, note, false
	}
	round, err := addRepeatSpread(ctx, gc, jf, rc, res, repeat, crit)
	if err != nil {
		return nil, strings.TrimSpace(note + " repeat: " + err.Error()), true
	}
	return round, note, false
}

// addRepeatSpread judges the round repeat-1 more times, fills each judge-scored criterion's Spread (the
// judge's own noise on a fixed answer, which no single verdict shows) and returns the round's own.
func addRepeatSpread(ctx context.Context, gc vetting.Config, jf vetting.JudgeFactory, rc vetting.ReplayCase, first vetting.ReplayRoundResult, repeat int, crit []ReplayCriterionReport) (*ReplaySpread, error) {
	samples := map[string][]float64{}
	for _, c := range first.Criteria {
		if !c.Deterministic {
			samples[c.Name] = append(samples[c.Name], c.Score)
		}
	}
	roundPassed := []float64{passedAll(first)}
	for i := 1; i < repeat; i++ {
		res, err := vetting.ReplayRound(ctx, gc, jf, rc)
		if err != nil {
			return nil, fmt.Errorf("pass %d: %w", i+1, err)
		}
		roundPassed = append(roundPassed, passedAll(res))
		for _, c := range res.Criteria {
			if _, ok := samples[c.Name]; ok {
				samples[c.Name] = append(samples[c.Name], c.Score)
			}
		}
	}
	for i := range crit {
		if xs := samples[crit[i].Name]; len(xs) >= 2 {
			crit[i].Spread = spreadOf(xs, first.Threshold)
		}
	}
	round := spreadOf(roundPassed, 1)
	round.Samples = nil // 0/1 pass flags; PassRate and Agreement carry them
	return round, nil
}

// passedAll is 1 when every criterion of res clears its threshold (the gate's weakest-link pass), else 0.
func passedAll(res vetting.ReplayRoundResult) float64 {
	for _, c := range res.Criteria {
		if c.Score < res.Threshold {
			return 0
		}
	}
	return 1
}

// spreadOf summarises xs, counting a sample at or above threshold as a pass.
func spreadOf(xs []float64, threshold float64) *ReplaySpread {
	var passes int
	for _, x := range xs {
		if x >= threshold {
			passes++
		}
	}
	rate := float64(passes) / float64(len(xs))
	rounded := make([]float64, len(xs))
	for i, x := range xs {
		rounded[i] = round4(x)
	}
	return &ReplaySpread{N: len(xs), Samples: rounded, Mean: round4(mean(xs)), SD: round4(stddev(xs)),
		Min: round4(slices.Min(xs)), Max: round4(slices.Max(xs)), PassRate: round4(rate), Agreement: round4(max(rate, 1-rate))}
}

// round4 keeps a stored report diffable: float noise past 4 places is not a change.
func round4(x float64) float64 {
	return math.Round(x*1e4) / 1e4
}

func mean(xs []float64) float64 {
	var t float64
	for _, x := range xs {
		t += x
	}
	return t / float64(len(xs))
}

// stddev is the population standard deviation; --repeat samples are the whole set, not a draw.
func stddev(xs []float64) float64 {
	m, v := mean(xs), 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return math.Sqrt(v / float64(len(xs)))
}

func unitRows(checks []vetting.UnitCheck) []UnitRow {
	rows := make([]UnitRow, 0, len(checks))
	for _, c := range checks {
		rows = append(rows, UnitRow{Kind: c.Unit.Kind, Unit: clip(c.Unit.Text, 160), Specific: c.Specific.Value,
			Citation: c.Citation, State: c.State, Window: clip(c.Window, 200), Verify: c.Verdict.State, Quote: clip(c.Verdict.Quote, 200)})
	}
	return rows
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n-- // never split a rune
	}
	return s[:n] + "..."
}

// renderUnits prints the shadow tier's tally and only the rows a judge would need to see.
func renderUnits(out io.Writer, rows []UnitRow) {
	if len(rows) == 0 {
		return
	}
	tally := map[string]int{}
	for _, r := range rows {
		tally[r.State]++
	}
	fmt.Fprintf(out, "  units: %d specific(s): located=%d unlocated=%d uncited=%d no_stored_text=%d\n",
		len(rows), tally["located"], tally["unlocated"], tally["uncited"], tally["no_stored_text"])
	verify := map[string]int{}
	for _, r := range rows {
		if r.Verify != "" {
			verify[r.Verify]++
		}
	}
	if len(verify) > 0 {
		fmt.Fprintf(out, "  verify: supported=%d unsupported=%d cannot_tell=%d not_checked=%d\n",
			verify["supported"], verify["unsupported"], verify["cannot_tell"], verify["not_checked"])
	}
	for _, r := range rows {
		switch {
		case r.Verify == "unsupported":
			fmt.Fprintf(out, "    unsupported    %-24q in %s\n      evidence: %s\n", r.Specific, clip(r.Unit, 100), clip(r.Quote, 120))
		case r.State != "located":
			fmt.Fprintf(out, "    %-14s %-24q in %s\n", r.State, r.Specific, clip(r.Unit, 100))
		}
	}
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

func renderReplayReports(out io.Writer, report ReplayReport) {
	for _, r := range report.Rounds {
		fmt.Fprintf(out, "%s %s\n", r.Node, r.Round)
		if r.Skipped != "" {
			fmt.Fprintf(out, "  skipped: %s\n", r.Skipped)
			continue
		}
		if r.Note != "" {
			fmt.Fprintf(out, "  note: %s\n", r.Note)
		}
		renderUnits(out, r.Units)
		for _, c := range r.Criteria {
			flag := " "
			if c.Flipped {
				flag = "!"
			}
			fmt.Fprintf(out, "  %s %-24s recorded=%-12s replayed=%-12s rubric_mark=%.2f threshold=%.2f  %s\n",
				flag, c.Name, scoreCell(c.HasRecorded, c.RecordedScore, c.RecordedPassed),
				scoreCell(c.HasReplayed, c.ReplayedScore, c.ReplayedPassed), c.RubricMark, c.Threshold, c.Reason)
			if sp := c.Spread; sp != nil {
				fmt.Fprintf(out, "    spread n=%d mean=%.2f sd=%.2f min=%.2f max=%.2f pass=%.0f%% agreement=%.0f%%\n",
					sp.N, sp.Mean, sp.SD, sp.Min, sp.Max, 100*sp.PassRate, 100*sp.Agreement)
			}
		}
		if sp := r.Spread; sp != nil {
			fmt.Fprintf(out, "  round n=%d pass=%.0f%% agreement=%.0f%%\n", sp.N, 100*sp.PassRate, 100*sp.Agreement)
		}
	}
	if c := report.Consistency; c != nil {
		fmt.Fprintf(out, "consistency: %d round(s), %d criterion cell(s), %d unstable; criterion agreement %.0f%%, round agreement %.0f%%, max sd %.2f\n",
			c.Rounds, c.Criteria, c.Unstable, 100*c.CriterionAgreement, 100*c.RoundAgreement, c.MaxSD)
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

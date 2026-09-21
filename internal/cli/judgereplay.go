package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/adk/v2/model"

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
type ReplayCriterionReport struct {
	Name           string  `json:"name"`
	RecordedScore  float64 `json:"recorded_score"`
	ReplayedScore  float64 `json:"replayed_score"`
	PassMark       float64 `json:"pass_mark"`
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
	Skipped  string                  `json:"skipped,omitempty"` // set instead of Criteria when this round couldn't be rebuilt
	// ArtifactsWritten: this round wrote to these artifacts - the recording
	// bundle carries no artifact body, so a criterion the live judge scored by
	// reading one (e.g. cites_sources on a delivered-by-artifact answer) may not reproduce.
	ArtifactsWritten []string `json:"artifacts_written,omitempty"`
}

// judgedRound is one (node, judge round) this bundle recorded a verdict for.
type judgedRound struct {
	node, agent, judgeRound string
	roundNum                int
}

// judgedRounds finds every (node, judge round) sess recorded eval.score
// entries for, filtered by opts, oldest node/round first.
func judgedRounds(sess *bundle.Session, opts ReplayOptions) []judgedRound {
	byKey := map[[2]string]bool{}
	for _, sc := range sess.EvaluationResults() {
		byKey[[2]string{sc.Node, sc.Round}] = true
	}
	agentOf := map[string]string{}
	for _, k := range sess.Streams() {
		if k.Node != "" && k.Agent != "" && k.Agent != "judge" {
			agentOf[k.Node] = k.Agent
		}
	}
	out := make([]judgedRound, 0, len(byKey))
	for k := range byKey {
		node, round := k[0], k[1]
		if opts.Node != "" && node != opts.Node {
			continue
		}
		n, ok := judgeRoundNum(round)
		if !ok {
			continue
		}
		if opts.Round != 0 && n != opts.Round {
			continue
		}
		out = append(out, judgedRound{node: node, agent: agentOf[node], judgeRound: round, roundNum: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].node != out[j].node {
			return out[i].node < out[j].node
		}
		return out[i].roundNum < out[j].roundNum
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

// workerTurnsFor rebuilds jr's worker activity input: the graded round's
// task/answer, and one RawTurn (each round's LAST recorded llm.call) per
// worker round up to and including it (worker-r0 .. worker-r(roundNum-1)).
func workerTurnsFor(sess *bundle.Session, jr judgedRound) (task, answer string, turns []vetting.RawTurn, ok bool) {
	workerRound := fmt.Sprintf("worker-r%d", jr.roundNum-1)
	key := bundle.StreamKey{Node: jr.node, Agent: jr.agent, Round: workerRound}
	task, answer, _, ok = sess.RoundTaskAnswer(key)
	if !ok {
		return "", "", nil, false
	}
	for i := 0; i <= jr.roundNum-1; i++ {
		rk := bundle.StreamKey{Node: jr.node, Agent: jr.agent, Round: fmt.Sprintf("worker-r%d", i)}
		ct := sess.ChatTurns(rk)
		if len(ct) == 0 {
			continue
		}
		last := ct[len(ct)-1]
		turns = append(turns, vetting.RawTurn{Input: last.Input, Output: last.Output})
	}
	return task, answer, turns, true
}

// recordedFor returns jr's recorded per-criterion scores.
func recordedFor(sess *bundle.Session, jr judgedRound) map[string]float64 {
	out := map[string]float64{}
	for _, sc := range sess.EvaluationResults() {
		if sc.Node == jr.node && sc.Round == jr.judgeRound {
			out[sc.Criterion] = sc.Score
		}
	}
	return out
}

// question is the whole chat's original request - the judge prompt's
// BACKGROUND section; a node's own task is scored separately (workerTurnsFor).
func question(sess *bundle.Session) string {
	turns := sess.UserTurns()
	if len(turns) == 0 {
		return ""
	}
	return turns[0]
}

// RubricConfigFor builds cfg's gate Config for one agent bundle: the same
// base+bundle-override rubric resolution serve.go's perAgentGateCfg does,
// plus ReadOnly/RequireRetrieval approximated from the agent's own ac.Tools.
func RubricConfigFor(ctx context.Context, cfg *config.Config, agentName, rubricOverride string) (vetting.Config, error) {
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
	base.RubricSpecs, base.RubricFixes, base.RubricPassMarks = rr.Specs, rr.Fixes, rr.PassMarks
	base.ReadOnly = true
	for _, tn := range ac.Tools {
		if tn == "git_push" {
			base.ReadOnly = false
		}
		if tn == "web_search" || tn == "web_fetch" {
			base.RequireRetrieval = true
		}
	}
	return base, nil
}

// BuildReplayJudge builds the local quack.yaml's configured judge model as a
// JudgeFactory with no read/skill tools - a replayed round reads only the
// recording, never a live repo or skill store. nil, nil, nil when the gate has no judge configured.
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

// RunJudgeReplay replays every judged round opts selects from sess, printing
// (or, asJSON, encoding) each round's recorded-vs-replayed comparison. Returns
// a non-zero exit code the moment any criterion's pass/fail flips.
func RunJudgeReplay(ctx context.Context, cfg *config.Config, sess *bundle.Session, opts ReplayOptions, judge vetting.JudgeFactory, out io.Writer, asJSON bool) int {
	q := question(sess)
	rounds := judgedRounds(sess, opts)
	reports := make([]ReplayRoundReport, 0, len(rounds))
	exit := 0
	cfgCache := map[string]vetting.Config{}
	for _, jr := range rounds {
		rep := ReplayRoundReport{Node: jr.node, Agent: jr.agent, Round: jr.judgeRound}
		task, answer, turns, ok := workerTurnsFor(sess, jr)
		if !ok {
			rep.Skipped = fmt.Sprintf("no recorded worker-r%d round to rebuild this judge round's answer from", jr.roundNum-1)
			reports = append(reports, rep)
			exit = maxInt(exit, 1)
			continue
		}
		gc, ok := cfgCache[jr.agent]
		if !ok {
			var err error
			gc, err = RubricConfigFor(ctx, cfg, jr.agent, opts.RubricPath)
			if err != nil {
				rep.Skipped = err.Error()
				reports = append(reports, rep)
				exit = maxInt(exit, 1)
				continue
			}
			cfgCache[jr.agent] = gc
		}
		jf := judge
		if opts.DeterministicOnly {
			jf = nil
		}
		criteria, artifactsWritten, err := vetting.ReplayRound(ctx, gc, jf, vetting.ReplayCase{NodeID: jr.node, Task: task, Question: q, Answer: answer, WorkerTurns: turns})
		if err != nil {
			rep.Skipped = err.Error()
			reports = append(reports, rep)
			exit = maxInt(exit, 1)
			continue
		}
		rep.ArtifactsWritten = artifactsWritten
		recorded := recordedFor(sess, jr)
		rep.Criteria = compareCriteria(criteria, recorded)
		for _, c := range rep.Criteria {
			if c.Flipped {
				rep.Flipped = true
			}
		}
		if rep.Flipped {
			exit = 2
		}
		reports = append(reports, rep)
	}
	if asJSON {
		_ = WriteJSON(out, reports)
	} else {
		renderReplayReports(out, reports)
	}
	return exit
}

func compareCriteria(criteria []vetting.ReplayCriterion, recorded map[string]float64) []ReplayCriterionReport {
	out := make([]ReplayCriterionReport, 0, len(criteria))
	for _, c := range criteria {
		rec, hasRec := recorded[c.Name]
		recPass := hasRec && rec >= c.PassMark
		replPass := c.Score >= c.PassMark
		out = append(out, ReplayCriterionReport{
			Name: c.Name, RecordedScore: rec, ReplayedScore: c.Score, PassMark: c.PassMark,
			RecordedPassed: recPass, ReplayedPassed: replPass, Deterministic: c.Deterministic,
			Reason: c.Reason, Flipped: hasRec && recPass != replPass,
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
		if len(r.ArtifactsWritten) > 0 {
			fmt.Fprintf(out, "  note: wrote artifact(s) %v this round - their content isn't in the recording bundle, so any criterion the judge scored by reading one may not reproduce\n", r.ArtifactsWritten)
		}
		for _, c := range r.Criteria {
			flag := " "
			if c.Flipped {
				flag = "!"
			}
			fmt.Fprintf(out, "  %s %-24s recorded=%.2f(%s) replayed=%.2f(%s) pass_mark=%.2f  %s\n",
				flag, c.Name, c.RecordedScore, passWord(c.RecordedPassed), c.ReplayedScore, passWord(c.ReplayedPassed), c.PassMark, c.Reason)
		}
	}
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

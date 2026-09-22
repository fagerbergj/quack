package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fagerbergj/quack/internal/stream"
	"sort"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// RawTurn is one recorded llm.call's Input/Output, verbatim JSON as the
// ledger stored them - one entry per contributing round or stream.
type RawTurn struct {
	Input, Output string
}

// ReplayCase is one judged round's gate inputs. Task is the built prompt
// live sends the worker - the same text also stands in for the judge's question.
type ReplayCase struct {
	NodeID, Task, Answer string
	WorkerTurns          []RawTurn
}

// ReplayCriterion is one criterion's freshly computed score. RubricMark is its
// own declared scale.pass, informational - ReplayRoundResult.Threshold decides pass/fail.
type ReplayCriterion struct {
	Name          string
	Score         float64
	RubricMark    float64
	Deterministic bool
	Reason        string
}

// ReplayRoundResult is one round's replayed criteria plus Threshold, the
// single global pass bar buildEnvelope actually gates on (envelope.go).
type ReplayRoundResult struct {
	Criteria         []ReplayCriterion
	Threshold        float64
	ArtifactsWritten []string
}

// ReplayRound re-scores rc under cfg's rubric via the live gate's own functions -
// judge nil replays deterministic criteria only, never touching a model.
func ReplayRound(ctx context.Context, cfg Config, judge JudgeFactory, rc ReplayCase) (ReplayRoundResult, error) {
	cfg.Task = rc.Task
	// Live judges the worker's reply after stream.StripThinking (runWorkerNode);
	// the recording holds the raw output, so strip it here or clean_output diverges.
	rc.Answer = stream.StripThinking(rc.Answer)
	act, err := rebuildWorkerActivity(ctx, rc.WorkerTurns, rc.NodeID)
	if err != nil {
		return ReplayRoundResult{}, err
	}
	augmentFromAnswer(&act, cfg, rc.Answer)
	res := ReplayRoundResult{Threshold: cfg.Threshold, ArtifactsWritten: act.artifactsWritten}

	det, _ := computeDeterministicCriteria(ctx, rc.Answer, act, cfg, rc.NodeID, time.Time{})
	// Environment-only failures here are the replay host's, not the round's; the
	// live judge never saw them, so they must not reach the judge prompt either.
	judgeDet := map[string]criterionScore{}
	for name, c := range det {
		if !EnvironmentOnlyCriteria[name] {
			judgeDet[name] = c
		}
	}

	v := verdict{}
	if judge != nil {
		question := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: rc.Task}}}
		v, err = runJudgeAgent(ctx, judge, cfg, question, rc.Answer, act, judgeDet, nil, func(*genai.Part) bool { return true })
		if err != nil {
			return res, fmt.Errorf("vetting: replay judge round: %w", err)
		}
	}
	v = mergeDeterministic(v, det, cfg)
	v = applyRubricSpecs(v, cfg.RubricSpecs)

	names := make([]string, 0, len(v.Criteria))
	for name := range v.Criteria {
		names = append(names, name)
	}
	sort.Strings(names) // stable report order, so two replays of one chat diff cleanly
	res.Criteria = make([]ReplayCriterion, 0, len(names))
	for _, name := range names {
		c := v.Criteria[name]
		res.Criteria = append(res.Criteria, ReplayCriterion{
			Name: name, Score: c.Score, RubricMark: cfg.RubricPassMarks[name],
			Deterministic: c.Deterministic, Reason: criterionText(c),
		})
	}
	return res, nil
}

// RebuildActivityArtifacts rebuilds rc's worker activity and returns only the
// artifacts it wrote - cheaper than ReplayRound for that decision alone.
func RebuildActivityArtifacts(ctx context.Context, cfg Config, rc ReplayCase) ([]string, error) {
	act, err := rebuildWorkerActivity(ctx, rc.WorkerTurns, rc.NodeID)
	if err != nil {
		return nil, err
	}
	augmentFromAnswer(&act, cfg, rc.Answer)
	return act.artifactsWritten, nil
}

// rebuildWorkerActivity replays turns' recorded contents through a fresh
// in-memory session, scanned by the gate's own activityFromSessionAt.
func rebuildWorkerActivity(ctx context.Context, turns []RawTurn, nodeDir string) (workerActivity, error) {
	svc := session.InMemoryService()
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: "judge-replay", UserID: "replay", SessionID: "replay"})
	if err != nil {
		return workerActivity{}, fmt.Errorf("vetting: replay session: %w", err)
	}
	sess := resp.Session
	for _, t := range turns {
		for _, c := range decodeContents(t.Input) {
			if err := appendReplayContent(ctx, svc, sess, c); err != nil {
				return workerActivity{}, err
			}
		}
		if c := decodeContent(t.Output); c != nil {
			if err := appendReplayContent(ctx, svc, sess, c); err != nil {
				return workerActivity{}, err
			}
		}
	}
	return activityFromSessionAt(sess, nodeDir), nil
}

func appendReplayContent(ctx context.Context, svc session.Service, sess session.Session, c *genai.Content) error {
	ev := session.NewEvent(ctx, "judge-replay")
	ev.Content = c
	return svc.AppendEvent(ctx, sess, ev)
}

// decodeContents parses a recorded llm.call Input string (a JSON array of
// genai.Content); malformed input yields no events, never a fatal error.
func decodeContents(raw string) []*genai.Content {
	if raw == "" {
		return nil
	}
	var contents []*genai.Content
	if json.Unmarshal([]byte(raw), &contents) != nil {
		return nil
	}
	return contents
}

// decodeContent parses a recorded llm.call Output string (one genai.Content).
func decodeContent(raw string) *genai.Content {
	if raw == "" {
		return nil
	}
	var c genai.Content
	if json.Unmarshal([]byte(raw), &c) != nil {
		return nil
	}
	return &c
}

// CountingJudgeFactory wraps factory, incrementing *calls on invocation -
// lets a caller outside this package prove a judge factory was never reached.
func CountingJudgeFactory(factory JudgeFactory, calls *int) JudgeFactory {
	return func(prompt judgePrompt, sink *verdict, forced *bool, maxIters, maxOutputTokens int, thinkingLevel string, receivedIDs []string, artifactTools []tool.Tool) (adkagent.Agent, judgeReadCounters, error) {
		*calls++
		return factory(prompt, sink, forced, maxIters, maxOutputTokens, thinkingLevel, receivedIDs, artifactTools)
	}
}

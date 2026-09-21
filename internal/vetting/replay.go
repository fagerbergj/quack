package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// RawTurn is one recorded llm.call's Input/Output - one per worker round
// (the round's LAST recorded call, whose Input already carries that round's
// whole tool-loop history), verbatim JSON as the ledger stored them.
type RawTurn struct {
	Input, Output string
}

// ReplayCase is one judged round's gate inputs, rebuilt from a recording:
// the node's task, the chat's question, the graded answer, and the worker's
// rounds up to and including this one (oldest first, for worker activity).
type ReplayCase struct {
	NodeID, Task, Question, Answer string
	WorkerTurns                    []RawTurn
}

// ReplayCriterion is one criterion's freshly computed score and rubric pass
// mark; the caller pairs it with the round's recorded score to decide flips.
type ReplayCriterion struct {
	Name          string
	Score         float64
	PassMark      float64
	Deterministic bool
	Reason        string
}

// ReplayRound re-scores rc under cfg's rubric via the live gate's own functions
// (computeDeterministicCriteria, judge non-nil's runJudgeAgent, mergeDeterministic,
// applyRubricSpecs) - judge nil replays deterministic criteria only, never touching a model.
func ReplayRound(ctx context.Context, cfg Config, judge JudgeFactory, rc ReplayCase) (criteria []ReplayCriterion, artifactsWritten []string, err error) {
	act, err := rebuildWorkerActivity(ctx, rc.WorkerTurns, rc.NodeID)
	if err != nil {
		return nil, nil, err
	}
	augmentFromAnswer(&act, cfg, rc.Answer)
	// The recording carries no artifact body, only its pointer - a criterion
	// the live judge scored by reading one may not reproduce; flag it.
	artifactsWritten = act.artifactsWritten

	det, _ := computeDeterministicCriteria(ctx, rc.Answer, act, cfg, rc.NodeID, time.Time{})

	v := verdict{}
	if judge != nil {
		question := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: rc.Question}}}
		v, err = runJudgeAgent(ctx, judge, cfg, question, rc.Answer, act, det, nil, func(*genai.Part) bool { return true })
		if err != nil {
			return nil, artifactsWritten, fmt.Errorf("vetting: replay judge round: %w", err)
		}
	}
	v = mergeDeterministic(v, det, cfg)
	v = applyRubricSpecs(v, cfg.RubricSpecs)

	names := make([]string, 0, len(v.Criteria))
	for name := range v.Criteria {
		names = append(names, name)
	}
	out := make([]ReplayCriterion, 0, len(names))
	for _, name := range names {
		c := v.Criteria[name]
		pass := cfg.Threshold
		if pm, ok := cfg.RubricPassMarks[name]; ok {
			pass = pm
		}
		out = append(out, ReplayCriterion{Name: name, Score: c.Score, PassMark: pass, Deterministic: c.Deterministic, Reason: criterionText(c)})
	}
	return out, artifactsWritten, nil
}

// rebuildWorkerActivity replays turns' recorded contents through a fresh
// in-memory ADK session and scans it with activityFromSessionAt - the
// gate's own event walk, not a reimplementation of it.
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
// genai.Content); malformed or empty input yields no events, never a fatal replay error.
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

// decodeContent parses a recorded llm.call Output string (a single genai.Content).
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
// lets a caller outside this package prove a judge factory is never
// reached, without needing its unexported per-round call types itself.
func CountingJudgeFactory(factory JudgeFactory, calls *int) JudgeFactory {
	return func(prompt judgePrompt, sink *verdict, forced *bool, maxIters, maxOutputTokens int, thinkingLevel string, receivedIDs []string, artifactTools []tool.Tool) (adkagent.Agent, judgeReadCounters, error) {
		*calls++
		return factory(prompt, sink, forced, maxIters, maxOutputTokens, thinkingLevel, receivedIDs, artifactTools)
	}
}

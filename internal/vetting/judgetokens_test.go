package vetting

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// usageTurn is a finished model turn reporting out output and thought tokens.
func usageTurn(r *model.LLMResponse, out, thought int32) (*model.LLMResponse, error) {
	r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{CandidatesTokenCount: out, ThoughtsTokenCount: thought}
	return r, nil
}

func toolCallTurn(name string) *model.LLMResponse { return stubCall(name, map[string]any{}) }

func judgeTokenCfg(t *testing.T) Config {
	t.Helper()
	artTools, err := NewJudgeArtifactTools(recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Rubric: "score 0-10", JudgeArtifactTools: artTools}
	cfg.judgeTiming = newJudgeRoundTiming()
	return cfg
}

func runTokenJudge(t *testing.T, cfg Config, llm fnLLM) error {
	t.Helper()
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "q"}}}
	_, err := runJudgeAgent(t.Context(), NewJudgeFactory(llm, nil, nil), cfg, q, "answer", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	return err
}

// TestJudgeTokensSumAcrossTurns: a tool-call turn then the verdict turn, usage on each, add up.
func TestJudgeTokensSumAcrossTurns(t *testing.T) {
	cfg := judgeTokenCfg(t)
	var n atomic.Int32
	err := runTokenJudge(t, cfg, func(*model.LLMRequest) (*model.LLMResponse, error) {
		if n.Add(1) == 1 {
			return usageTurn(toolCallTurn("list_artifacts"), 10, 1)
		}
		return usageTurn(stubCall(submitVerdictTool, map[string]any{"score": 3, "feedback": ""}), 20, 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 || cfg.judgeTiming.outTokens != 30 || cfg.judgeTiming.thoughtTokens != 3 {
		t.Errorf("model calls %d, tokens out %d thought %d; want 2 turns summed to 30 and 3", n.Load(), cfg.judgeTiming.outTokens, cfg.judgeTiming.thoughtTokens)
	}
}

// TestJudgeTokensSumAcrossRetriedSessions: a session that dies on a transient fault still spent its tokens.
func TestJudgeTokensSumAcrossRetriedSessions(t *testing.T) {
	cfg := judgeTokenCfg(t)
	var n atomic.Int32
	err := runTokenJudge(t, cfg, func(*model.LLMRequest) (*model.LLMResponse, error) {
		switch n.Add(1) {
		case 1:
			return usageTurn(toolCallTurn("list_artifacts"), 10, 1)
		case 2:
			return nil, errors.New("openai model (generate): status 503: unavailable")
		}
		return usageTurn(stubCall(submitVerdictTool, map[string]any{"score": 3, "feedback": ""}), 20, 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 || cfg.judgeTiming.outTokens != 30 || cfg.judgeTiming.thoughtTokens != 3 {
		t.Errorf("model calls %d, tokens out %d thought %d; want both sessions summed to 30 and 3", n.Load(), cfg.judgeTiming.outTokens, cfg.judgeTiming.thoughtTokens)
	}
}

// TestReplayRoundKeepsJudgeCostOnError: a failed judge call still reports the time and tokens it spent.
func TestReplayRoundKeepsJudgeCostOnError(t *testing.T) {
	cfg := replayGroundedCfg()
	artTools, err := NewJudgeArtifactTools(recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.JudgeArtifactTools = artTools
	var n atomic.Int32
	llm := fnLLM(func(*model.LLMRequest) (*model.LLMResponse, error) {
		if n.Add(1) == 1 {
			return usageTurn(toolCallTurn("list_artifacts"), 10, 1)
		}
		return nil, errors.New("status 400: bad request")
	})
	res, err := ReplayRound(context.Background(), cfg, NewJudgeFactory(llm, nil, nil), ReplayCase{NodeID: "n1", Task: "t", Answer: "a"})
	if err == nil {
		t.Fatal("ReplayRound err = nil, want the judge's failure")
	}
	if res.Judge == nil || res.Judge.OutputTokens != 10 || res.Judge.ThoughtTokens != 1 || res.Judge.Seconds <= 0 {
		t.Errorf("judge cost = %+v, want the failed call's 10 output, 1 thought tokens and its wall time", res.Judge)
	}
}

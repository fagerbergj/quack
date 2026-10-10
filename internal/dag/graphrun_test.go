// Package dag_test holds stub-model and graph-run helpers for tests that need the real
// dag.Executor/gate machinery.
package dag_test

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Stub-model helpers; dag's g* equivalents are unexported to this external package.

func atText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func atCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func atHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func atAllText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// fnLLM is a func-backed model.LLM: each call yields the one response fn returns.
type fnLLM func(context.Context, *model.LLMRequest) *model.LLMResponse

func (fnLLM) Name() string { return "fnLLM" }

func (f fnLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(f(ctx, req), nil) }
}

// textLLM answers every call with s.
func textLLM(s string) fnLLM {
	return func(context.Context, *model.LLMRequest) *model.LLMResponse { return atText(s) }
}

// passJudge passes every verdict.
var passJudge = fnLLM(func(context.Context, *model.LLMRequest) *model.LLMResponse {
	return atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""})
})

// failOnceJudge fails the first verdict (forcing one revise round) and passes every later one.
func failOnceJudge(score float64, feedback string) fnLLM {
	var calls atomic.Int32
	return func(context.Context, *model.LLMRequest) *model.LLMResponse {
		if calls.Add(1) == 1 {
			return atCall("submit_verdict", map[string]any{"score": score, "feedback": feedback})
		}
		return atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""})
	}
}

// dateToolLLM calls current_date once, then answers reply; as judge it passes.
func dateToolLLM(reply string) fnLLM {
	return func(_ context.Context, req *model.LLMRequest) *model.LLMResponse {
		switch {
		case atHasTool(req, "submit_verdict"):
			return atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": "fine"})
		case atHasFuncResponse(req, "current_date"):
			return atText(reply)
		}
		return atCall("current_date", map[string]any{})
	}
}

func atHasFuncResponse(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return true
			}
		}
	}
	return false
}

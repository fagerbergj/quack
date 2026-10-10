package dag

import (
	"context"
	"iter"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// stubG routes by agent role and the judge's submit_verdict tool; the synthesizer echoes its
// prompt, so a pass proves both researcher outputs reached it via JoinNode + buildTask.
type stubG struct{}

func (stubG) Name() string { return "stubG" }

func (stubG) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		sys := gSysText(req)
		switch {
		case strings.Contains(sys, "ROLE:r1"):
			yield(gText("ALPHA-FINDING"), nil)
		case strings.Contains(sys, "ROLE:r2"):
			yield(gText("BETA-FINDING"), nil)
		case strings.Contains(sys, "ROLE:synth"):
			yield(gText("SYNTH{"+gUserText(req)+"}"), nil)
		default:
			yield(gText("?"), nil)
		}
	}
}

func gHasTool(req *model.LLMRequest, name string) bool {
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

func gSysText(req *model.LLMRequest) string {
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	return gContentText(req.Config.SystemInstruction)
}

func gUserText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		b.WriteString(gContentText(c))
		b.WriteByte('\n')
	}
	return b.String()
}

func gContentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && !p.Thought && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func gText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func gCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

// fnLLM is a func-backed model.LLM: each call yields the one response fn returns.
type fnLLM func(context.Context, *model.LLMRequest) *model.LLMResponse

func (fnLLM) Name() string { return "fnLLM" }

func (f fnLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(f(ctx, req), nil) }
}

// fixedLLM answers every worker call with reply (after wait, if non-nil) and passes every judge round.
func fixedLLM(reply string, wait <-chan struct{}) fnLLM {
	return func(_ context.Context, req *model.LLMRequest) *model.LLMResponse {
		if gHasTool(req, "submit_verdict") {
			return gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""})
		}
		if wait != nil {
			<-wait
		}
		return gText(reply)
	}
}

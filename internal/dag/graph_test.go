package dag

import (
	"context"
	"iter"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// stubG is a deterministic model.LLM routing by agent role (system-instruction
// marker) and by the presence of the judge's submit_verdict tool. Researchers
// return distinct findings; the judge always passes; the synthesizer echoes the
// prompt it received - so a passing test proves both researcher outputs reached
// the synthesizer via JoinNode fan-in + buildTask assembly keyed by node ID.
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

// --- stub helpers ---

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

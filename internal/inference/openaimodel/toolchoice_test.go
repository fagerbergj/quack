package openaimodel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

// TestApplyConfigKnobs_ToolChoiceNone: a NONE function-calling mode keeps the tools
// declared (the prompt head is unchanged) and sends tool_choice "none".
func TestApplyConfigKnobs_ToolChoiceNone(t *testing.T) {
	tools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "submit_verdict", Description: "d"}}}}
	body := func(cfg *genai.GenerateContentConfig) string {
		var req openai.ChatCompletionNewParams
		if err := applyConfigKnobs(&req, cfg); err != nil {
			t.Fatalf("applyConfigKnobs: %v", err)
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	none := &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}}
	got := body(&genai.GenerateContentConfig{Tools: tools, ToolConfig: none})
	if !strings.Contains(got, `"tool_choice":"none"`) || !strings.Contains(got, "submit_verdict") {
		t.Errorf("request = %s, want the tool declared and tool_choice none", got)
	}
	if got := body(&genai.GenerateContentConfig{Tools: tools}); strings.Contains(got, "tool_choice") {
		t.Errorf("request = %s, want no tool_choice without a NONE mode", got)
	}
}

func TestConvertSchema_LowercasesNestedTypes(t *testing.T) {
	m, err := convertSchema(&genai.Schema{Type: genai.TypeObject, Required: []string{"q"},
		Properties: map[string]*genai.Schema{"q": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}}}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	if want := `{"properties":{"q":{"items":{"type":"string"},"type":"array"}},"required":["q"],"type":"object"}`; string(b) != want {
		t.Fatalf("convertSchema = %s, want %s", b, want)
	}
}

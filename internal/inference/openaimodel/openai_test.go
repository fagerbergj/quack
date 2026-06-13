package openaimodel

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// TestConvertToolsLowercasesGenaiSchemaTypes guards the fix for ADK tools (e.g.
// load_memory) that declare parameters via genai.Schema instead of
// ParametersJsonSchema. The raw genai.Schema marshals uppercase JSON-Schema
// types ("OBJECT"/"STRING"), which llama.cpp's tool grammar rejects — failing
// the whole request. convertTools must route the fallback through convertSchema
// so the emitted schema uses lowercase types.
func TestConvertToolsLowercasesGenaiSchemaTypes(t *testing.T) {
	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "load_memory",
			Description: "Loads the memory for the current user.",
			Parameters: &genai.Schema{ // genai.Schema, NOT ParametersJsonSchema
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"query": {Type: genai.TypeString, Description: "The query to search memory for."},
				},
				Required: []string{"query"},
			},
		}},
	}}

	got, err := convertTools(tools)
	if err != nil {
		t.Fatalf("convertTools: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1", len(got))
	}

	// The emitted parameters must contain no uppercase genai type tokens.
	b, err := json.Marshal(got[0].Function.Parameters)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	js := string(b)
	for _, bad := range []string{`"OBJECT"`, `"STRING"`} {
		if strings.Contains(js, bad) {
			t.Errorf("emitted schema contains uppercase type %s: %s", bad, js)
		}
	}
	if !strings.Contains(js, `"object"`) || !strings.Contains(js, `"string"`) {
		t.Errorf("emitted schema missing lowercase types: %s", js)
	}
}

// TestConvertToolsPrefersParametersJsonSchema confirms the existing path (used
// by quack's builtin functiontool-based tools) is unchanged: when
// ParametersJsonSchema is set it is passed through verbatim.
func TestConvertToolsPrefersParametersJsonSchema(t *testing.T) {
	jsonSchema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:                 "web_search",
			ParametersJsonSchema: jsonSchema,
		}},
	}}
	got, err := convertTools(tools)
	if err != nil {
		t.Fatalf("convertTools: %v", err)
	}
	if got[0].Function.Parameters == nil {
		t.Fatal("parameters dropped")
	}
}

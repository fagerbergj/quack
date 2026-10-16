package openaimodel

import (
	"iter"
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type stringArgsSchema struct {
	Criteria map[string]int `json:"criteria,omitempty"`
	Findings []string       `json:"findings,omitempty"`
	Note     string         `json:"note,omitempty"`
}

func TestDecodeStringArgs(t *testing.T) {
	schema, err := jsonschema.For[stringArgsSchema](nil)
	if err != nil {
		t.Fatal(err)
	}
	req := &model.LLMRequest{Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "submit", ParametersJsonSchema: schema}},
	}}}}
	call := func(name string, args map[string]any) *model.LLMResponse {
		return &model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}}}}
	}
	run := func(r *model.LLMResponse) map[string]any {
		seq := func(yield func(*model.LLMResponse, error) bool) { yield(r, nil) }
		for got := range decodeStringArgs(req, iter.Seq2[*model.LLMResponse, error](seq)) {
			return got.Content.Parts[0].FunctionCall.Args
		}
		return nil
	}

	got := run(call("submit", map[string]any{"criteria": `{"a": 1}`, "findings": `["x"]`, "note": `{"not": "decoded"}`}))
	want := map[string]any{"criteria": map[string]any{"a": float64(1)}, "findings": []any{"x"}, "note": `{"not": "decoded"}`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if got := run(call("submit", map[string]any{"criteria": `{"a": 1`})); !reflect.DeepEqual(got["criteria"], map[string]any{"a": float64(1)}) {
		t.Errorf("criteria missing its outer brace = %v, want the dropped closer restored", got["criteria"])
	}
	if got := run(call("submit", map[string]any{"criteria": `{"a": `})); got["criteria"] != `{"a": ` {
		t.Errorf("unrepairable criteria = %v, want it left as the string", got["criteria"])
	}
	if got := run(call("other", map[string]any{"criteria": `{"a": 1}`})); got["criteria"] != `{"a": 1}` {
		t.Errorf("undeclared tool args = %v, want untouched", got)
	}
}

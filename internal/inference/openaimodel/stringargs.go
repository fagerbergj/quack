package openaimodel

import (
	"encoding/json"
	"iter"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/model"
)

// decodeStringArgs json-decodes, in place, a tool-call argument whose declared type is object or array
// but arrived as a JSON string (OpenAI-compatible servers hand nested arguments back that way). An
// undecodable string stays, so the tool's schema check rejects it.
func decodeStringArgs(req *model.LLMRequest, seq iter.Seq2[*model.LLMResponse, error]) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range seq {
			if err == nil && resp != nil && resp.Content != nil {
				for _, p := range resp.Content.Parts {
					if p.FunctionCall != nil {
						decodeCallArgs(req, p.FunctionCall.Name, p.FunctionCall.Args)
					}
				}
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

func decodeCallArgs(req *model.LLMRequest, name string, args map[string]any) {
	if req.Config == nil {
		return
	}
	for _, t := range req.Config.Tools {
		for _, d := range t.FunctionDeclarations {
			if d.Name != name || d.ParametersJsonSchema == nil {
				continue
			}
			var s jsonschema.Schema
			if b, err := json.Marshal(d.ParametersJsonSchema); err != nil || json.Unmarshal(b, &s) != nil {
				return
			}
			for k, v := range args {
				str, ok := v.(string)
				p := s.Properties[k]
				if !ok || p == nil || !structured(p) {
					continue
				}
				if decoded, ok := decodeString(str); ok {
					args[k] = decoded
				}
			}
			return
		}
	}
}

// decodeString: ponytail: the trailing closer compensates for a dropped final "}" seen on 12% of prod judge
// verdicts; delete once the cause (model or tool parser) is fixed.
func decodeString(s string) (v any, ok bool) {
	for _, closer := range []string{"", "}", "]"} {
		if json.Unmarshal([]byte(s+closer), &v) == nil {
			return v, true
		}
	}
	return nil, false
}

func structured(s *jsonschema.Schema) bool {
	is := func(t string) bool { return t == "object" || t == "array" }
	return is(s.Type) || slices.ContainsFunc(s.Types, is)
}

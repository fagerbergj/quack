package vetting

import (
	"testing"

	"google.golang.org/genai"
)

func TestStagedCandidate(t *testing.T) {
	// happy path: content trimmed, kind carried as metadata
	c, ok := stagedCandidate(&genai.FunctionCall{Args: map[string]any{"content": "  a good source  ", "kind": "source"}})
	if !ok || c.Content != "a good source" || c.Metadata["kind"] != "source" {
		t.Fatalf("got %+v ok=%v", c, ok)
	}
	// kind optional
	if c, ok := stagedCandidate(&genai.FunctionCall{Args: map[string]any{"content": "x"}}); !ok || c.Metadata != nil {
		t.Fatalf("no-kind case: got %+v ok=%v", c, ok)
	}
	// blank / missing content is not staged (guards the arg-key contract)
	for _, args := range []map[string]any{{"content": "   "}, {}, {"content": 42}} {
		if _, ok := stagedCandidate(&genai.FunctionCall{Args: args}); ok {
			t.Errorf("args %v should not stage", args)
		}
	}
}

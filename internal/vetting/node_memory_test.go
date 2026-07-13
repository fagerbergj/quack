package vetting

import (
	"testing"

	"google.golang.org/genai"
)

func TestWorkerInput(t *testing.T) {
	// no attachments → plain string (text-only node)
	if got := workerInput("hi", nil); got != "hi" {
		t.Fatalf("no-attach: got %#v, want string \"hi\"", got)
	}
	// with attachments → *genai.Content: prompt first, then the media parts
	img := &genai.Part{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1, 2, 3}}}
	got := workerInput("describe this", []*genai.Part{img})
	c, ok := got.(*genai.Content)
	if !ok {
		t.Fatalf("with-attach: got %T, want *genai.Content", got)
	}
	if len(c.Parts) != 2 || c.Parts[0].Text != "describe this" || c.Parts[1].InlineData == nil {
		t.Fatalf("with-attach: parts = %+v", c.Parts)
	}
}

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
	// bucket (what the memory is ABOUT) is carried through: it routes the write to the
	// shared repo/role/user bucket (memory.Scope.writeBucket).
	c, ok = stagedCandidate(&genai.FunctionCall{Args: map[string]any{
		"content": "load-games.ts registers every game", "kind": "layout", "bucket": "repo",
	}})
	if !ok || c.Metadata["bucket"] != "repo" || c.Metadata["kind"] != "layout" {
		t.Fatalf("bucket case: got %+v ok=%v", c, ok)
	}
	// blank / missing content is not staged (guards the arg-key contract)
	for _, args := range []map[string]any{{"content": "   "}, {}, {"content": 42}} {
		if _, ok := stagedCandidate(&genai.FunctionCall{Args: args}); ok {
			t.Errorf("args %v should not stage", args)
		}
	}
}

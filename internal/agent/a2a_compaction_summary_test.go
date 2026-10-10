package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// ADK's default MaxToolContentChars (2000) applies to every rendered part, including the rolling summary
// that seeds the next window; a summary past 2000 chars must reach the summarizer whole.
func TestNativeCompactionConfigDoesNotTruncatePreviousSummary(t *testing.T) {
	var last *model.LLMRequest
	m := &fakeLLM{func(req *model.LLMRequest) *model.LLMResponse {
		last = req
		return &model.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel), FinishReason: genai.FinishReasonStop}
	}}
	cfg, err := NativeCompactionConfig(Compaction{
		Summarizer:    m,
		ContextWindow: 131072,
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("NativeCompactionConfig: %v", err)
	}

	long := "## Goal\n" + strings.Repeat("- /very/long/path/file_x.go: func Foo(a int) error {...}\n", 400)
	if len(long) < 22000 {
		t.Fatalf("test fixture too short to prove no truncation: %d chars", len(long))
	}

	prev := &session.EventCompaction{
		StartTimestamp:   time.Now().Add(-time.Hour),
		EndTimestamp:     time.Now().Add(-time.Minute),
		CompactedContent: genai.NewContentFromText(long, genai.RoleModel),
	}
	seed := &session.Event{
		ID: "seed", Author: "model", Timestamp: prev.StartTimestamp,
		LLMResponse: model.LLMResponse{Content: prev.CompactedContent},
		Actions:     session.EventActions{Compaction: prev},
	}
	user := &session.Event{
		ID: "u1", Author: "user", Timestamp: time.Now(),
		LLMResponse: model.LLMResponse{Content: genai.NewContentFromText("next question", genai.RoleUser)},
	}

	if _, err := cfg.Summarizer.SummarizeEvents(context.Background(), []*session.Event{seed, user}); err != nil {
		t.Fatalf("SummarizeEvents: %v", err)
	}
	if last == nil || len(last.Contents) == 0 || len(last.Contents[0].Parts) == 0 {
		t.Fatal("summarizer model was never called with a prompt")
	}
	got := last.Contents[0].Parts[0].Text
	if strings.Contains(got, "[truncated") {
		t.Fatalf("previous summary was truncated despite MaxToolContentChars: -1: %.200s...", got)
	}
	// adk's formatEvents escapes newlines so a value can't forge a line break; compare against the same escaping.
	wantEscaped := strings.ReplaceAll(long, "\n", "\\n")
	if !strings.Contains(got, wantEscaped) {
		t.Fatalf("summarizer prompt does not contain the full previous summary verbatim; got %d chars", len(got))
	}
}

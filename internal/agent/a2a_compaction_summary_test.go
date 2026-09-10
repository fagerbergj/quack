package agent

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// capturingSummaryModel records the last LLMRequest a summarizer sent it.
type capturingSummaryModel struct{ last *model.LLMRequest }

func (m *capturingSummaryModel) Name() string { return "summarizer" }

func (m *capturingSummaryModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.last = req
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel), FinishReason: genai.FinishReasonStop}, nil)
	}
}

// TestNativeCompactionConfigDoesNotTruncatePreviousSummary is a regression test for the ADK audit's A1 finding: NativeCompactionConfig left MaxToolContentChars
// at ADK's zero value, which the summarizer reads as its 2000-char default and applies to EVERY rendered part - including the rolling summary that tail
// retention seeds the next compaction window with. quack's compaction prompt demands "ALL TECHNICAL CONTENT" and allows a summary far longer than usual, so a summary that grows past 2000 chars must reach the summarizer whole.
func TestNativeCompactionConfigDoesNotTruncatePreviousSummary(t *testing.T) {
	m := &capturingSummaryModel{}
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
	if m.last == nil || len(m.last.Contents) == 0 || len(m.last.Contents[0].Parts) == 0 {
		t.Fatal("summarizer model was never called with a prompt")
	}
	got := m.last.Contents[0].Parts[0].Text
	if strings.Contains(got, "[truncated") {
		t.Fatalf("previous summary was truncated despite MaxToolContentChars: -1: %.200s...", got)
	}
	// formatEvents escapes newlines to literal "\n" so a rendered value can't
	// forge a line break (session/compaction/llm_summarizer.go escapeLines) -
	// compare against the same escaping rather than the raw text.
	wantEscaped := strings.ReplaceAll(long, "\n", "\\n")
	if !strings.Contains(got, wantEscaped) {
		t.Fatalf("summarizer prompt does not contain the full previous summary verbatim; got %d chars", len(got))
	}
}

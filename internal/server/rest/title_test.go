package rest

import (
	"context"
	"iter"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference/openaimodel"
)

// fixedAnswerModel always answers with a fixed piece of text - used here to
// stand in for a DAG run's synthesizer answer.
type fixedAnswerModel struct{ text string }

func (fixedAnswerModel) Name() string { return "fixed" }

func (m fixedAnswerModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: m.text}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// erroringModel always fails with a dial-shaped error, recording whether the
// ctx it was called with carried openaimodel.WithBestEffort.
type erroringModel struct{ sawBestEffort *bool }

func (erroringModel) Name() string { return "erroring" }

func (m erroringModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	*m.sawBestEffort = openaimodel.IsBestEffort(ctx)
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})
	}
}

func TestGenerateTitle_MarksCtxBestEffort(t *testing.T) {
	var sawBestEffort bool
	h := &Handler{titler: erroringModel{sawBestEffort: &sawBestEffort}}
	title := h.generateTitle(context.Background(), "chat-1", "hello")
	if title != "" {
		t.Errorf("title = %q, want empty on titler error", title)
	}
	if !sawBestEffort {
		t.Error("generateTitle's ctx did not carry openaimodel.WithBestEffort")
	}
}

// TestGenerateTitle_DialFailureLogsBelowDefaultLevel: a dial failure logs at
// Debug (invisible at the default Info level), not Warn.
func TestGenerateTitle_DialFailureLogsBelowDefaultLevel(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	h := &Handler{titler: erroringModel{sawBestEffort: new(bool)}}
	h.generateTitle(context.Background(), "chat-1", "hello")

	if strings.Contains(buf.String(), "title generation failed") {
		t.Errorf("dial failure should not log at the default level, got:\n%s", buf.String())
	}
}

// TestSanitizeTitle_RejectsNonCompliantAnswerLikeResponse is #1124's QA
// evidence, verbatim: a titler that ignores its instruction and answers the
// underlying question instead of titling it produces a markdown-decorated, multi-line response. sanitizeTitle must reduce it to a short, plain, one-line fragment - never let the full explanation through.
func TestSanitizeTitle_RejectsNonCompliantAnswerLikeResponse(t *testing.T) {
	raw := "**Researcher Node:**\nIn a write-ahead-log (WAL) database, a checkpoint is a mechanism that " +
		"flushes dirty in-memory pages to disk so the log can be truncated, bounding crash-recovery replay time."
	got := sanitizeTitle(raw)
	if strings.Contains(got, "\n") {
		t.Fatalf("sanitizeTitle(%q) = %q, want a single line", raw, got)
	}
	if strings.ContainsAny(got, "*#`_") {
		t.Fatalf("sanitizeTitle(%q) = %q, want markdown stripped", raw, got)
	}
	if n := len(strings.Fields(got)); n > fallbackTitleWords {
		t.Fatalf("sanitizeTitle(%q) = %q, has %d words, want at most %d", raw, got, n, fallbackTitleWords)
	}
	if strings.Contains(got, "checkpoint is a mechanism") {
		t.Fatalf("sanitizeTitle(%q) = %q, leaked the answer body", raw, got)
	}
}

// TestGenerateTitle_NonCompliantTitlerFallsBackShort exercises the full
// generateTitle path (system instruction + sanitizeTitle) with a titler stub
// that reproduces the exact QA-observed non-compliant shape.
func TestGenerateTitle_NonCompliantTitlerFallsBackShort(t *testing.T) {
	answer := "**Researcher Node:**\nIn a write-ahead-log (WAL) database, a checkpoint is a mechanism that " +
		"flushes dirty in-memory pages to disk so the log can be truncated, bounding crash-recovery replay time."
	h := &Handler{titler: fixedAnswerModel{text: answer}}
	title := h.generateTitle(context.Background(), "chat-1124", "what is a WAL checkpoint?")
	if title == "" {
		t.Fatal("generateTitle returned empty for a non-compliant (but non-erroring) titler response")
	}
	if strings.Contains(title, "\n") || strings.ContainsAny(title, "*#`_") {
		t.Fatalf("title = %q, want single-line plain text", title)
	}
	if n := len(strings.Fields(title)); n > fallbackTitleWords {
		t.Fatalf("title = %q has %d words, want at most %d", title, n, fallbackTitleWords)
	}
}

// TestRunChat_TitlerFailureFallsBackToShortUserDerivedTitle is #1124's
// regression guard: a run whose titler is unavailable (nil, here - the same
// shape as errored/empty in generateTitle) must never leave the chat titled with its own answer, and must never leave it titled at all forever - it gets a short title derived from the user's own request instead. newTestHandlerWithModel wires titler: nil, so generateTitle short-circuits exactly like a real titler failure would.
func TestRunChat_TitlerFailureFallsBackToShortUserDerivedTitle(t *testing.T) {
	answer := "# WAL Design Comparison: PostgreSQL vs. SQLite\n\n## Researcher Node A — " +
		strings.Repeat("detailed findings ", 50)
	h := newTestHandlerWithModel(t, fixedAnswerModel{text: answer})
	chatID := mustCreateChat(t, h)

	postMessage(t, h, chatID, "compare WAL designs for postgres vs sqlite please")

	c, err := h.store.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if c.Title == "" {
		t.Fatal("Title left empty - a titler failure must fall back to a derived title, not silence")
	}
	if c.Title == answer || strings.Contains(c.Title, "Researcher Node A") {
		t.Fatalf("Title = %q, want a short title derived from the user's request, never the run's own answer", c.Title)
	}
	if n := len(strings.Fields(c.Title)); n > fallbackTitleWords {
		t.Fatalf("Title has %d words, want at most %d", n, fallbackTitleWords)
	}
}

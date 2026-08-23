package inference

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

func withTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exp
}

func spanAttrsOf(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// TestTracedModel_DecoratesSpanBeforeADKEndsIt pins the ordering trap: ADK's
// generate_content span is ended synchronously the instant a non-partial
// response is handed to its own range-loop body - here simulated by ending
// the span immediately inside tracedModel's yield callback, exactly where
// ADK's real base_flow.go does it. If setResponseSpanAttrs ran in a deferred
// emit (after ADK's End()) instead of before yield, this test would fail with
// no output-message attribute recorded - SetAttributes on an ended span is a
// silent no-op, so a naive regression would NOT panic, it would just vanish.
func TestTracedModel_DecoratesSpanBeforeADKEndsIt(t *testing.T) {
	exp := withTestTracer(t)

	ctx, span := otel.Tracer("test").Start(context.Background(), "generate_content test-model")
	tm := &tracedModel{
		LLM: &stubModel{name: "m", resps: []*model.LLMResponse{
			{Content: &genai.Content{Parts: []*genai.Part{{Text: "the answer"}}}},
		}},
		name: "m",
	}
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}},
	}
	for resp, err := range tm.GenerateContent(ctx, req, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.Partial {
			// The exact moment ADK's base_flow.go ends its span.
			span.End()
		}
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	attrs := spanAttrsOf(spans[0])
	if got := attrs["gen_ai.input.messages"]; !strings.Contains(got, `"hi"`) {
		t.Errorf("gen_ai.input.messages = %q, want to contain the request text", got)
	}
	if got := attrs["gen_ai.output.messages"]; !strings.Contains(got, "the answer") {
		t.Errorf("gen_ai.output.messages = %q, want to contain the response text (attrs set after End() are silently dropped)", got)
	}
	if got := attrs["langfuse.observation.output"]; !strings.Contains(got, "the answer") {
		t.Errorf("langfuse.observation.output = %q, want to contain the response text", got)
	}
}

// TestSetRequestSpanAttrs_Redacts proves content reaching a span attribute
// passes the same key-name redaction the log path gets via
// ledger.NewRedactingProcessor - span attributes never flow through that
// processor, so redactedSpanAttr is the only thing standing between a
// secret-keyed field and an exported OTLP span.
func TestSetRequestSpanAttrs_Redacts(t *testing.T) {
	exp := withTestTracer(t)

	ctx, span := otel.Tracer("test").Start(context.Background(), "generate_content test-model")
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{
			Text: `{"api_key":"sk-super-secret","note":"hello"}`,
		}}}},
	}
	setRequestSpanAttrs(ctx, req)
	span.End()

	attrs := spanAttrsOf(exp.GetSpans()[0])
	got := attrs["gen_ai.input.messages"]
	if strings.Contains(got, "sk-super-secret") {
		t.Fatalf("gen_ai.input.messages leaked an unredacted secret: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("gen_ai.input.messages = %q, want the api_key field redacted", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("gen_ai.input.messages = %q, want the non-secret field preserved", got)
	}
}

// TestSetRequestSpanAttrs_ConversationID reuses ledger.Coords for
// gen_ai.conversation.id rather than inventing a new lookup.
func TestSetRequestSpanAttrs_ConversationID(t *testing.T) {
	exp := withTestTracer(t)

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-123"})
	ctx, span := otel.Tracer("test").Start(ctx, "generate_content test-model")
	setRequestSpanAttrs(ctx, &model.LLMRequest{})
	span.End()

	if got := spanAttrsOf(exp.GetSpans()[0])["gen_ai.conversation.id"]; got != "chat-123" {
		t.Errorf("gen_ai.conversation.id = %q, want chat-123", got)
	}
}

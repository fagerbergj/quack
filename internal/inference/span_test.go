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
	"github.com/fagerbergj/quack/internal/otelobs"
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

// withContentCapture flips otelobs' shared capture-content flag for the
// duration of one test, restoring the previous value on cleanup.
func withContentCapture(t *testing.T, enabled bool) {
	t.Helper()
	prev := otelobs.CaptureContentEnabled()
	otelobs.SetCaptureContent(enabled)
	t.Cleanup(func() { otelobs.SetCaptureContent(prev) })
}

func spanAttrsOf(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// ADK ends the span inside the yield of a non-partial response (simulated here); attributes set
// after that are silently dropped, so a deferred emit would lose the output message.
func TestTracedModel_DecoratesSpanBeforeADKEndsIt(t *testing.T) {
	withContentCapture(t, true)
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

// Spans never pass through the log RedactingProcessor, so redactedSpanAttr is the only guard
// between a secret-keyed field and an exported span.
func TestSetRequestSpanAttrs_Redacts(t *testing.T) {
	withContentCapture(t, true)
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
	withContentCapture(t, true)
	exp := withTestTracer(t)

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-123"})
	ctx, span := otel.Tracer("test").Start(ctx, "generate_content test-model")
	setRequestSpanAttrs(ctx, &model.LLMRequest{})
	span.End()

	if got := spanAttrsOf(exp.GetSpans()[0])["gen_ai.conversation.id"]; got != "chat-123" {
		t.Errorf("gen_ai.conversation.id = %q, want chat-123", got)
	}
}

// A store-backed prompt stamps the resolved artifact name (not the agent key) and version
// under Langfuse's observation.prompt.* keys.
func TestSetRequestSpanAttrs_LangfusePromptLink(t *testing.T) {
	withContentCapture(t, false)
	exp := withTestTracer(t)

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{
		ChatID: "chat-123", Agent: "reviewer", PromptSource: "prod-langfuse", PromptArtifact: "system/reviewer", PromptVersionID: "7",
	})
	ctx, span := otel.Tracer("test").Start(ctx, "generate_content test-model")
	setRequestSpanAttrs(ctx, &model.LLMRequest{})
	span.End()

	attrs := spanAttrsOf(exp.GetSpans()[0])
	if got := attrs["langfuse.observation.prompt.name"]; got != "system/reviewer" {
		t.Errorf("langfuse.observation.prompt.name = %q, want system/reviewer", got)
	}
	if got := attrs["langfuse.observation.prompt.version"]; got != "7" {
		t.Errorf("langfuse.observation.prompt.version = %q, want 7", got)
	}
}

// TestSetRequestSpanAttrs_NonLangfuseSourceSkipsPromptLink proves a static
// (or other) prompt source never gets the langfuse.observation.prompt.* pair.
func TestSetRequestSpanAttrs_NonLangfuseSourceSkipsPromptLink(t *testing.T) {
	withContentCapture(t, false)
	exp := withTestTracer(t)

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{
		ChatID: "chat-123", Agent: "reviewer", PromptSource: "static", PromptArtifact: "system/reviewer", PromptVersionID: "abc",
	})
	ctx, span := otel.Tracer("test").Start(ctx, "generate_content test-model")
	setRequestSpanAttrs(ctx, &model.LLMRequest{})
	span.End()

	attrs := spanAttrsOf(exp.GetSpans()[0])
	if _, ok := attrs["langfuse.observation.prompt.name"]; ok {
		t.Error("langfuse.observation.prompt.name present for a static-sourced prompt, want absent")
	}
}

// With content capture unset (the default), no message content reaches a recording span.
func TestSpanAttrs_ContentCaptureOffByDefault(t *testing.T) {
	withContentCapture(t, false)
	exp := withTestTracer(t)

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-123"})
	ctx, span := otel.Tracer("test").Start(ctx, "generate_content test-model")
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}},
	}
	setRequestSpanAttrs(ctx, req)
	setResponseSpanAttrs(ctx, &model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "the answer"}}}})
	span.End()

	attrs := spanAttrsOf(exp.GetSpans()[0])
	for _, k := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "langfuse.observation.input", "langfuse.observation.output"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("attribute %q present with content capture off, want absent", k)
		}
	}
	// conversation.id is a correlation key, not content - stays ungated (see setRequestSpanAttrs).
	if got := attrs["gen_ai.conversation.id"]; got != "chat-123" {
		t.Errorf("gen_ai.conversation.id = %q, want chat-123 (should not be gated by content capture)", got)
	}
}

// ADK never says which node ran its span; node/agent are correlation keys, so they survive with
// content capture off.
func TestSetRequestSpanAttrs_NodeAndAgentSurviveContentGate(t *testing.T) {
	withContentCapture(t, false)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	ctx, span := tp.Tracer("t").Start(ledger.WithCoords(context.Background(), ledger.Coords{
		ChatID: "chat-1", Node: "node-2", Agent: "web-researcher",
	}), "generate_content m")
	setRequestSpanAttrs(ctx, &model.LLMRequest{})
	span.End()

	got := map[string]string{}
	for _, kv := range exp.GetSpans()[0].Attributes {
		got[string(kv.Key)] = kv.Value.String()
	}
	if got[otelobs.QuackNode] != "node-2" {
		t.Errorf("quack.node = %q, want node-2 (trace can't be filtered to one node without it)", got[otelobs.QuackNode])
	}
	if got[otelobs.GenAIAgentName] != "web-researcher" {
		t.Errorf("gen_ai.agent.name = %q, want web-researcher", got[otelobs.GenAIAgentName])
	}
	if got[otelobs.GenAIConversationID] != "chat-1" {
		t.Errorf("gen_ai.conversation.id = %q, want chat-1", got[otelobs.GenAIConversationID])
	}
}

// A non-slug node id could be message-derived and sits outside the content gate, so it isn't exported.
func TestSetRequestSpanAttrs_NonSlugNodeIDIsNotExported(t *testing.T) {
	withContentCapture(t, false)
	for _, node := range []string{
		"summarize the user's bank details for acct 1234",
		strings.Repeat("x", 65),
		"has spaces",
	} {
		exp := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
		ctx, span := tp.Tracer("t").Start(ledger.WithCoords(context.Background(),
			ledger.Coords{ChatID: "c1", Node: node}), "generate_content m")
		setRequestSpanAttrs(ctx, &model.LLMRequest{})
		span.End()
		for _, kv := range exp.GetSpans()[0].Attributes {
			if string(kv.Key) == otelobs.QuackNode {
				t.Errorf("node %q was exported as %s; want it withheld", node, kv.Key)
			}
		}
	}
}

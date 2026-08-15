package otelobs

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
)

func TestInit_DisabledIsNoop(t *testing.T) {
	disabled := false
	p, shutdown, err := Init(context.Background(), config.ObservabilityConfig{Otel: config.OtelConfig{Enabled: &disabled}}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.TracerProvider != nil || p.MeterProvider != nil || p.LoggerProvider != nil {
		t.Errorf("expected a no-op Providers when disabled, got %+v", p)
	}
	// Start must still be safe to call (no-op span), and shutdown must not error.
	_, span := Start(context.Background(), "run")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestInit_EnabledInstallsGlobalProviders(t *testing.T) {
	p, shutdown, err := Init(context.Background(), config.ObservabilityConfig{Otel: config.OtelConfig{Sample: 1.0}}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if p.TracerProvider == nil || p.MeterProvider == nil || p.LoggerProvider == nil {
		t.Fatalf("expected real providers when enabled, got %+v", p)
	}

	// Spans/metrics must be safe to record - emission-only, no local read-back.
	_, span := Start(context.Background(), "run")
	End(span, nil)

	RunStarted()
	RecordJudgeVerdict("code-implementer", 0.9, true)
	RecordDeliveryOutcome(DeliveryDelivered)
	RecordRoundDuration("code-implementer", "qwen3-coder", "worker", 100*time.Millisecond)
	RunFinished()

	// EmitLog must be safe to call unconditionally, same as Start/Record*.
	EmitLog(context.Background(), "test", "hello")
}

func TestTraceIDOf(t *testing.T) {
	if got := TraceIDOf(context.Background()); got != "" {
		t.Errorf("TraceIDOf(no span) = %q, want empty", got)
	}
	_, shutdown, err := Init(context.Background(), config.ObservabilityConfig{Otel: config.OtelConfig{Sample: 1.0}}, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	ctx, span := Start(context.Background(), "run")
	defer span.End()
	if got := TraceIDOf(ctx); got == "" {
		t.Errorf("TraceIDOf(spanned ctx) = empty, want a trace id")
	}
}

// withTestTracer installs an in-memory span recorder as the global tracer
// provider for the test's duration and returns it for assertions.
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

func spanAttrs(t *testing.T, exp *tracetest.InMemoryExporter, name string) map[string]string {
	t.Helper()
	for _, s := range exp.GetSpans() {
		if s.Name != name {
			continue
		}
		attrs := map[string]string{}
		for _, kv := range s.Attributes {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		return attrs
	}
	t.Fatalf("span %q was never recorded (got %d spans)", name, len(exp.GetSpans()))
	return nil
}

// TestStart_EmitsConversationIDAlongsideChatID: Langfuse (and OTel-native
// tooling generally) derives a trace's session from gen_ai.conversation.id and
// never from a vendor key, so every quack span must carry it - while chat_id
// stays for the queries already written against it.
func TestStart_EmitsConversationIDAlongsideChatID(t *testing.T) {
	exp := withTestTracer(t)

	_, span := Start(context.Background(), "run", attribute.String(ChatIDKey, "ext:github:quack-919"))
	span.End()

	attrs := spanAttrs(t, exp, "quack.run")
	if got := attrs[GenAIConversationID]; got != "ext:github:quack-919" {
		t.Errorf("%s = %q, want the chat id", GenAIConversationID, got)
	}
	if got := attrs[ChatIDKey]; got != "ext:github:quack-919" {
		t.Errorf("%s = %q, want it retained", ChatIDKey, got)
	}
}

// TestStart_FallsBackToLedgerCoords covers the spans that never pass chat_id
// explicitly (plan, acp.*): the identity is on ctx, same source EmitLog reads.
func TestStart_FallsBackToLedgerCoords(t *testing.T) {
	exp := withTestTracer(t)

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-1", User: "quack-auto-review"})
	_, span := Start(ctx, "plan")
	span.End()

	attrs := spanAttrs(t, exp, "quack.plan")
	if got := attrs[GenAIConversationID]; got != "chat-1" {
		t.Errorf("%s = %q, want chat-1 from ctx coords", GenAIConversationID, got)
	}
	if got := attrs[UserID]; got != "quack-auto-review" {
		t.Errorf("%s = %q, want quack-auto-review", UserID, got)
	}
}

// TestStartLinked_RootSpanCarriesConversationID: memory.commit opens its own
// trace, so an untagged root there is a whole trace with no session.
func TestStartLinked_RootSpanCarriesConversationID(t *testing.T) {
	exp := withTestTracer(t)

	_, span := StartLinked(context.Background(), "memory.commit", oteltrace.SpanContext{},
		attribute.String(ChatIDKey, "chat-1"))
	span.End()

	if got := spanAttrs(t, exp, "quack.memory.commit")[GenAIConversationID]; got != "chat-1" {
		t.Errorf("%s = %q, want chat-1 on the linked root span", GenAIConversationID, got)
	}
}

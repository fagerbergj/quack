package vetting

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// withRecordingSpan returns a context with a recording span and a func that
// ends it and exports it, so the caller controls when the exporter sees it.
func withRecordingSpan(t *testing.T) (context.Context, func(), *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); otel.SetTracerProvider(prev) })
	ctx, span := tp.Tracer("t").Start(context.Background(), "node")
	return ctx, func() { span.End() }, exp
}

func outputAttr(exp *tracetest.InMemoryExporter) (string, bool) {
	spans := exp.GetSpans()
	if len(spans) == 0 {
		return "", false
	}
	for _, kv := range spans[0].Attributes {
		if string(kv.Key) == "langfuse.observation.output" {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// TestSetDeliveryOutputAttr proves the delivered text lands on the enclosing
// (node root) span as langfuse.observation.output, gated the same way every
// other content span attribute is.
func TestSetDeliveryOutputAttr(t *testing.T) {
	prev := otelobs.CaptureContentEnabled()
	otelobs.SetCaptureContent(true)
	t.Cleanup(func() { otelobs.SetCaptureContent(prev) })

	ctx, end, exp := withRecordingSpan(t)
	setDeliveryOutputAttr(ctx, DeliveryContext{Items: []StagedDelivery{{Body: "the answer"}}})
	end()
	if got, ok := outputAttr(exp); !ok || got != "the answer" {
		t.Fatalf("langfuse.observation.output = %q, ok=%v, want %q, true", got, ok, "the answer")
	}
}

func TestSetDeliveryOutputAttr_ContentCaptureOff(t *testing.T) {
	prev := otelobs.CaptureContentEnabled()
	otelobs.SetCaptureContent(false)
	t.Cleanup(func() { otelobs.SetCaptureContent(prev) })

	ctx, end, exp := withRecordingSpan(t)
	setDeliveryOutputAttr(ctx, DeliveryContext{Items: []StagedDelivery{{Body: "the answer"}}})
	end()
	if _, ok := outputAttr(exp); ok {
		t.Fatal("langfuse.observation.output present with content capture off, want absent")
	}
}

func TestSetDeliveryOutputAttr_NoBodies(t *testing.T) {
	prev := otelobs.CaptureContentEnabled()
	otelobs.SetCaptureContent(true)
	t.Cleanup(func() { otelobs.SetCaptureContent(prev) })

	ctx, end, exp := withRecordingSpan(t)
	setDeliveryOutputAttr(ctx, DeliveryContext{Items: []StagedDelivery{{}}})
	end()
	if _, ok := outputAttr(exp); ok {
		t.Fatal("langfuse.observation.output present with no item bodies, want absent")
	}
}

package otelobs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestWrapHandler_AddsTraceAttrsWhenSpanned(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(WrapHandler(slog.NewJSONHandler(&buf, nil)))

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("test").Start(context.Background(), "quack.run")
	defer span.End()

	logger.InfoContext(ctx, "hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if rec["trace_id"] == nil || rec["trace_id"] == "" {
		t.Errorf("expected trace_id attr, got %v", rec)
	}
	if rec["span_id"] == nil || rec["span_id"] == "" {
		t.Errorf("expected span_id attr, got %v", rec)
	}
}

func TestWrapHandler_PassthroughWhenNoSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(WrapHandler(slog.NewJSONHandler(&buf, nil)))

	logger.Info("hello")

	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("expected no trace_id attr for a spanless ctx, got %q", buf.String())
	}
}

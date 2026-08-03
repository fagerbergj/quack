package otelobs

import (
	"context"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
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

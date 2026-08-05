package otelobs

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
)

// Tracer is the one tracer every span starts from.
const tracerName = "github.com/fagerbergj/quack"

// ServiceName is the OTel resource's service.name prefix.
const ServiceName = "quack"

// ChatIDKey is the standard span attribute for chat/run in scope.
const ChatIDKey = "chat_id"

// Providers holds the process-wide OTel wiring; emission-only - Grafana owns viewing.
// KNOWN LIMITATION: ADK's own internal spans do not flow through this provider.
type Providers struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *metric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider
}

// Init builds tracer+meter+logger providers and installs globals; disabled returns no-ops.
func Init(ctx context.Context, cfg config.ObservabilityConfig, ledgerStore ledger.LedgerStore) (*Providers, func(context.Context) error, error) {
	if !cfg.Otel.IsEnabled() {
		return &Providers{}, func(context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(ServiceName),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("otelobs: resource: %w", err)
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Otel.Sample))),
	}
	mpOpts := []metric.Option{metric.WithResource(res)}
	var shutdowns []func(context.Context) error
	if cfg.Otel.OTLPEndpoint != "" {
		texp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Otel.OTLPEndpoint))
		if err != nil {
			return nil, nil, fmt.Errorf("otelobs: otlp trace exporter: %w", err)
		}
		bsp := sdktrace.NewBatchSpanProcessor(texp)
		tpOpts = append(tpOpts, sdktrace.WithSpanProcessor(bsp))
		shutdowns = append(shutdowns, bsp.Shutdown)

		mexp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(cfg.Otel.OTLPEndpoint))
		if err != nil {
			return nil, nil, fmt.Errorf("otelobs: otlp metric exporter: %w", err)
		}
		periodic := metric.NewPeriodicReader(mexp)
		mpOpts = append(mpOpts, metric.WithReader(periodic))
		shutdowns = append(shutdowns, periodic.Shutdown)
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)

	mp := metric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(mp)

	if err := initMetrics(mp.Meter(tracerName)); err != nil {
		// best-effort observability, not a startup dependency.
		logf("metric instrument init failed; metrics disabled", "err", err)
	}

	lp, logShutdown, err := initLogs(ctx, res, cfg, ledgerStore)
	if err != nil {
		return nil, nil, err
	}
	shutdowns = append(shutdowns, logShutdown)

	providers := &Providers{TracerProvider: tp, MeterProvider: mp, LoggerProvider: lp}
	shutdown := func(sctx context.Context) error {
		var firstErr error
		for _, fn := range append([]func(context.Context) error{tp.Shutdown, mp.Shutdown}, shutdowns...) {
			if err := fn(sctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	return providers, shutdown, nil
}

// tracer reads otel.GetTracerProvider() lazily so disabled config yields no-ops.
func tracer() oteltrace.Tracer { return otel.Tracer(tracerName) }

// Start opens "quack.<name>" as a child span; safe to call unconditionally.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return tracer().Start(ctx, "quack."+name, oteltrace.WithAttributes(attrs...))
}

// StartLinked opens a NEW ROOT span linked to (not child of) linkTo for async work.
func StartLinked(ctx context.Context, name string, linkTo oteltrace.SpanContext, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	opts := []oteltrace.SpanStartOption{oteltrace.WithAttributes(attrs...), oteltrace.WithNewRoot()}
	if linkTo.IsValid() {
		opts = append(opts, oteltrace.WithLinks(oteltrace.Link{SpanContext: linkTo}))
	}
	return tracer().Start(ctx, "quack."+name, opts...)
}

// End closes span, recording err as status/exception.
func End(span oteltrace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// TraceIDOf returns the trace id of ctx's active span for cross-referencing.
func TraceIDOf(ctx context.Context) string {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

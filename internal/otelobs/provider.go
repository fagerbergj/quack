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

// Tracer is the one tracer every Quack span is started from.
const tracerName = "github.com/fagerbergj/quack"

// ServiceName is the OTel resource's service.name - the vocabulary prefix
// every span name below also carries ("quack.<thing>").
const ServiceName = "quack"

// ChatIDKey is the standard span attribute name every quack span that has a
// chat/run in scope carries it under.
const ChatIDKey = "chat_id"

// Providers holds the process-wide OTel wiring: the tracer/meter providers,
// also installed as the otel package globals via otel.SetTracerProvider/
// SetMeterProvider. Emission-only - quack keeps no local trace/metric store
// or read API of its own; Tempo/Grafana (the home-server monitoring stack,
// staged behind otel.otlp_endpoint) own trace and metric viewing.
//
// KNOWN LIMITATION: ADK v2's own internal spans do NOT flow through this
// provider. internal/telemetry (ADK) captures its tracer at package-init
// time - `var tracer = otel.GetTracerProvider().Tracer(...)` - which runs
// before Init below ever executes, so it is permanently bound to the SDK's
// default no-op provider; ADK exposes no production API to rebind it (only
// a test-only OverrideTracerForTesting). Every span in this package is
// quack's own explicit instrumentation, not "free" ADK auto-instrumentation.
type Providers struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *metric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider
}

// Init builds the tracer + meter + logger providers per cfg and installs the
// tracer/meter as the otel package globals (the logger provider has no such
// global in this SDK version - see Logger). Disabled (cfg.Otel.IsEnabled() ==
// false) returns a no-op Providers so callers never need an `if enabled`
// branch of their own - every otelobs.Start/Record*/EmitLog call below is a
// cheap no-op against the SDK's default no-op providers. otlp_endpoint unset
// ⇒ providers are built (spans/metrics/logs still recorded) but nothing is
// EXPORTED anywhere - harmless, just inert; set it to actually ship to a
// collector. The replay ledger (cfg.Recording) rides this SAME logger
// provider, so it can only ever be active when otel itself is - see
// config.RecordingConfig.IsEnabled.
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
		// Metrics are best-effort observability, not a startup dependency.
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

// tracer is the process-wide tracer every Start call below uses. Reading
// otel.GetTracerProvider() lazily (rather than caching Init's tp) means a
// no-op TracerProvider (otel.enabled: false) yields a no-op tracer too, with
// no separate disabled-check needed at every call site.
func tracer() oteltrace.Tracer { return otel.Tracer(tracerName) }

// Start opens a span named "quack.<name>" as a child of ctx's current span (or
// a new trace root if ctx carries none), with the given attributes. Safe to
// call unconditionally - a disabled/no-op provider yields a no-op span whose
// End/SetAttributes/RecordError are all cheap no-ops.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return tracer().Start(ctx, "quack."+name, oteltrace.WithAttributes(attrs...))
}

// StartLinked opens a NEW ROOT span named "quack.<name>" linked to (not a
// child of) linkTo - for async/fire-and-forget work whose trigger may finish
// (and its own span end) before this work does, e.g. the gate's
// fire-and-forget memory commit. A zero/invalid linkTo yields a plain
// unlinked root span.
func StartLinked(ctx context.Context, name string, linkTo oteltrace.SpanContext, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	opts := []oteltrace.SpanStartOption{oteltrace.WithAttributes(attrs...), oteltrace.WithNewRoot()}
	if linkTo.IsValid() {
		opts = append(opts, oteltrace.WithLinks(oteltrace.Link{SpanContext: linkTo}))
	}
	return tracer().Start(ctx, "quack."+name, opts...)
}

// End closes span, recording err (if non-nil) as the span's status/exception.
func End(span oteltrace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// TraceIDOf returns the trace id of ctx's active span, "" if none (otel
// disabled or no span in context) - for cross-referencing a durable event log
// line (e.g. delivery_result) into Tempo/Grafana by hand.
func TraceIDOf(ctx context.Context) string {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

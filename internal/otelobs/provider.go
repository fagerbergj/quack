package otelobs

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/config"
)

// Tracer is the one tracer every Quack span is started from.
const tracerName = "github.com/fagerbergj/quack"

// ServiceName is the OTel resource's service.name — the vocabulary prefix
// every span name below also carries ("quack.<thing>").
const ServiceName = "quack"

// ChatIDKey is the standard span attribute name every quack span that has a
// chat/run in scope carries it under.
const ChatIDKey = "chat_id"

// Providers holds the process-wide OTel wiring: the tracer/meter providers,
// also installed as the otel package globals so ADK's own internal
// instrumentation flows through them for free. Emission-only: quack keeps no
// local trace/metric store of its own — Tempo/Grafana (the home-server
// monitoring stack, staged behind otel.otlp_endpoint) own trace and metric
// viewing. The durable event log (internal/store ChatEvents) is the
// zero-infra observability surface `quack obs` reads.
type Providers struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *metric.MeterProvider
}

// Init builds the tracer + meter providers per cfg and installs them as the
// otel package globals. Disabled (cfg.IsEnabled() == false) returns a no-op
// Providers so callers never need an `if enabled` branch of their own — every
// otelobs.Start/Record* call below is a cheap no-op against the SDK's default
// no-op global providers. otlp_endpoint unset ⇒ providers are built (spans are
// still recorded, metrics still accumulate) but nothing is EXPORTED anywhere
// — harmless, just inert; set it to actually ship to a collector.
func Init(ctx context.Context, cfg config.OtelConfig) (*Providers, func(context.Context) error, error) {
	if !cfg.IsEnabled() {
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
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Sample))),
	}
	mpOpts := []metric.Option{metric.WithResource(res)}
	var shutdowns []func(context.Context) error
	if cfg.OTLPEndpoint != "" {
		texp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint))
		if err != nil {
			return nil, nil, fmt.Errorf("otelobs: otlp trace exporter: %w", err)
		}
		bsp := sdktrace.NewBatchSpanProcessor(texp)
		tpOpts = append(tpOpts, sdktrace.WithSpanProcessor(bsp))
		shutdowns = append(shutdowns, bsp.Shutdown)

		mexp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(cfg.OTLPEndpoint))
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

	providers := &Providers{TracerProvider: tp, MeterProvider: mp}
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
// call unconditionally — a disabled/no-op provider yields a no-op span whose
// End/SetAttributes/RecordError are all cheap no-ops.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return tracer().Start(ctx, "quack."+name, oteltrace.WithAttributes(attrs...))
}

// StartLinked opens a NEW ROOT span named "quack.<name>" linked to (not a
// child of) linkTo — for async/fire-and-forget work whose trigger may finish
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
// disabled or no span in context) — for cross-referencing a durable event log
// line (e.g. delivery_result) into Tempo/Grafana by hand.
func TraceIDOf(ctx context.Context) string {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

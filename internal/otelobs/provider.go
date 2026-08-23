package otelobs

import (
	"context"
	"fmt"
	"strings"

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

// Resource attributes for "which build, which deployment" - neither has a
// semconv form quack can use here: v1.26 predates deployment.environment.name,
// and release is a Langfuse field with no semantic convention at all (its
// version field reads service.version, its release field only langfuse.release).
const (
	DeploymentEnvironmentName = "deployment.environment.name"
	langfuseRelease           = "langfuse.release"
)

// ChatIDKey is the input-side key callers pass to name the chat/run in scope;
// sessionAttrs consumes it and exports only gen_ai.conversation.id (what
// OTel-native tooling, e.g. Langfuse sessions, groups a trace by) - it never
// reaches the span itself.
const ChatIDKey = "chat_id"

// Providers holds the process-wide OTel wiring; emission-only - Grafana owns viewing.
// ADK's internal spans DO flow through it (the global TracerProvider delegates). The
// real gap is upstream: Runner.Run never opens its own span (runner.go:184).
type Providers struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *metric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider
}

// Init builds tracer+meter+logger providers and installs globals; disabled returns no-ops.

// signalURL pins the signal path instead of leaving it to the exporter's
// default. otlp*http 1.45 stopped appending it to a path-less endpoint and
// posts to / instead, which loses telemetry silently - and the deployed
// endpoint is path-less (http://otel-collector:4318). An endpoint that already
// names a path is left alone.
func signalURL(endpoint, path string) string {
	trimmed := strings.TrimRight(endpoint, "/")
	if i := strings.Index(trimmed, "://"); i >= 0 {
		if strings.Contains(trimmed[i+3:], "/") {
			return endpoint
		}
	} else if strings.Contains(trimmed, "/") {
		return endpoint
	}
	return trimmed + path
}

// newResource builds the resource every signal carries. version is the build
// stamp (serve.Version); a dev build leaves it empty and the attributes are
// omitted rather than exported as "".
func newResource(cfg config.OtelConfig, version string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(ServiceName)}
	if version != "" {
		attrs = append(attrs, semconv.ServiceVersion(version), attribute.String(langfuseRelease, version))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String(DeploymentEnvironmentName, cfg.Environment))
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
}

func Init(ctx context.Context, cfg config.ObservabilityConfig, ledgerStore ledger.LedgerStore, version string) (*Providers, func(context.Context) error, error) {
	if !cfg.Otel.IsEnabled() {
		return &Providers{}, func(context.Context) error { return nil }, nil
	}

	res, err := newResource(cfg.Otel, version)
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
		texp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(signalURL(cfg.Otel.OTLPEndpoint, "/v1/traces")))
		if err != nil {
			return nil, nil, fmt.Errorf("otelobs: otlp trace exporter: %w", err)
		}
		bsp := sdktrace.NewBatchSpanProcessor(texp)
		tpOpts = append(tpOpts, sdktrace.WithSpanProcessor(bsp))
		shutdowns = append(shutdowns, bsp.Shutdown)

		mexp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(signalURL(cfg.Otel.OTLPEndpoint, "/v1/metrics")))
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

// sessionAttrs adds gen_ai.conversation.id (from the explicit chat_id attr, else
// ctx coords - the same source EmitLog reads) and user.id, so every quack span
// carries the session identity OTel consumers resolve traces by. The bare
// chat_id attr callers pass is consumed here, not re-exported - gen_ai.conversation.id
// is the one identifier that reaches the span.
func sessionAttrs(ctx context.Context, attrs []attribute.KeyValue) []attribute.KeyValue {
	c := ledger.CoordsFromContext(ctx)
	chatID := c.ChatID
	out := attrs[:0:0]
	for _, kv := range attrs {
		if kv.Key == ChatIDKey {
			if kv.Value.AsString() != "" {
				chatID = kv.Value.AsString()
			}
			continue
		}
		out = append(out, kv)
	}
	attrs = out
	if chatID != "" {
		attrs = append(attrs, attribute.String(GenAIConversationID, chatID))
	}
	if c.User != "" {
		attrs = append(attrs, attribute.String(UserID, c.User))
	}
	return attrs
}

// Start opens "quack.<name>" as a child span; safe to call unconditionally.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	return tracer().Start(ctx, "quack."+name, oteltrace.WithAttributes(sessionAttrs(ctx, attrs)...))
}

// StartLinked opens a NEW ROOT span linked to (not child of) linkTo for async work.
func StartLinked(ctx context.Context, name string, linkTo oteltrace.SpanContext, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	opts := []oteltrace.SpanStartOption{oteltrace.WithAttributes(sessionAttrs(ctx, attrs)...), oteltrace.WithNewRoot()}
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

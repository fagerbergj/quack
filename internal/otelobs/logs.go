package otelobs

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
)

// GenAISemConvVersion pins the gen_ai attribute vocabulary every ledger
// entry is drawn from (the semconv package version below) - replay's
// divergence report keys off this pinned string, not a runtime-detected
// library version.
const GenAISemConvVersion = "1.41.0"

// Standard gen_ai.* attribute keys, drawn from OTel's own generated semconv
// package (go.opentelemetry.io/otel/semconv/v1.41.0) rather than hand-typed
// here - string(...) because log.KeyValue and attribute.KeyValue are
// distinct types across the trace/log APIs, but the underlying key names
// are the one vocabulary. gen_ai.prompt.version is the one exception: not
// yet a registered semconv attribute, so it stays a plain quack-defined
// string. The two custom quack.* attributes the replay-log design allows
// (.quack/replay-log.md) have no semconv equivalent by construction.
const (
	GenAIOperationName         = string(semconv.GenAIOperationNameKey)
	GenAIProviderName          = string(semconv.GenAIProviderNameKey)
	GenAIConversationID        = string(semconv.GenAIConversationIDKey)
	GenAIAgentName             = string(semconv.GenAIAgentNameKey)
	GenAIRequestModel          = string(semconv.GenAIRequestModelKey)
	GenAIRequestTemperature    = string(semconv.GenAIRequestTemperatureKey)
	GenAIRequestMaxTokens      = string(semconv.GenAIRequestMaxTokensKey)
	GenAIRequestSeed           = string(semconv.GenAIRequestSeedKey)
	GenAIResponseModel         = string(semconv.GenAIResponseModelKey)
	GenAIResponseID            = string(semconv.GenAIResponseIDKey)
	GenAIInputMessages         = string(semconv.GenAIInputMessagesKey)
	GenAIOutputMessages        = string(semconv.GenAIOutputMessagesKey)
	GenAISystemInstructions    = string(semconv.GenAISystemInstructionsKey)
	GenAIToolDefinitions       = string(semconv.GenAIToolDefinitionsKey)
	GenAIToolName              = string(semconv.GenAIToolNameKey)
	GenAIToolType              = string(semconv.GenAIToolTypeKey)
	GenAIToolCallArguments     = string(semconv.GenAIToolCallArgumentsKey)
	GenAIToolCallResult        = string(semconv.GenAIToolCallResultKey)
	GenAIPromptName            = string(semconv.GenAIPromptNameKey)
	GenAIPromptVersion         = "gen_ai.prompt.version" // not yet a registered semconv attribute
	GenAIWorkflowName          = string(semconv.GenAIWorkflowNameKey)
	GenAIEvaluationName        = string(semconv.GenAIEvaluationNameKey)
	GenAIEvaluationScore       = string(semconv.GenAIEvaluationScoreValueKey)
	GenAIEvaluationExplain     = string(semconv.GenAIEvaluationExplanationKey)
	GenAIUsageInputTokens      = string(semconv.GenAIUsageInputTokensKey)
	GenAIUsageOutputTokens     = string(semconv.GenAIUsageOutputTokensKey)
	GenAIResponseFinishReasons = string(semconv.GenAIResponseFinishReasonsKey)
	ErrorType                  = string(semconv.ErrorTypeKey)

	// GenAIOperationPlan is the planner/plan-tool seam's operation.name value -
	// unlike Chat/ExecuteTool/InvokeAgent (vars, below), "plan" has no
	// registered semconv enum to draw from.
	GenAIOperationPlan = "plan"

	quackNodeKey  = "quack.node"
	quackRoundKey = "quack.round"
)

// GenAIOperationChat/ExecuteTool/InvokeAgent are the semconv-enumerated
// gen_ai.operation.name values quack's three ADK seams map onto directly
// (inference, tools, acp) - vars, not consts, because attribute.Value has no
// constant form. "plan" (the planner/plan-tool seam, GenAIOperationPlan
// above) has no registered enum value.
var (
	GenAIOperationChat        = semconv.GenAIOperationNameChat.Value.AsString()
	GenAIOperationExecuteTool = semconv.GenAIOperationNameExecuteTool.Value.AsString()
	GenAIOperationInvokeAgent = semconv.GenAIOperationNameInvokeAgent.Value.AsString()

	// GenAIProviderOpenAI is quack's one implemented inference provider kind
	// (internal/inference/factory.go only implements "openai").
	GenAIProviderOpenAI = semconv.GenAIProviderNameOpenAI.Value.AsString()
)

// loggerProvider is the process-wide logger provider Init installs -
// mirrors tracer()'s lazy-global pattern, but logs have no stable global
// slot in this SDK version (go.opentelemetry.io/otel/log/global is a
// separate, still-evolving package), so quack keeps its own pointer.
var loggerProvider atomic.Pointer[sdklog.LoggerProvider]

// Logger returns the named otellog.Logger against the installed logger
// provider. Before Init runs, or when observability is disabled, this is a
// noop.Logger - every EmitLog call below is safe unconditionally.
func Logger(name string) otellog.Logger {
	if lp := loggerProvider.Load(); lp != nil {
		return lp.Logger(name)
	}
	return noop.NewLoggerProvider().Logger(name)
}

// LoggingEnabled reports whether scope's logger has anything listening -
// unlike a span's handful of attribute.String calls, a gen_ai event's
// payload (full message/tool-call content) is expensive enough to build
// that every emit*Event call site (inference/tools/acp/plan) checks this
// FIRST and skips the marshaling work entirely when recording and OTLP log
// export are both off, rather than building an event nobody will read.
func LoggingEnabled(scope string) bool {
	return Logger(scope).Enabled(context.Background(), otellog.EnabledParameters{})
}

// SetLoggerProviderForTesting overrides the process-wide logger provider -
// test-only, mirrors ADK's own OverrideTracerForTesting pattern (see the
// KNOWN LIMITATION note on Providers in provider.go). Callers restore the
// prior provider when done.
func SetLoggerProviderForTesting(lp *sdklog.LoggerProvider) (restore func()) {
	prev := loggerProvider.Load()
	loggerProvider.Store(lp)
	return func() { loggerProvider.Store(prev) }
}

// initLogs builds the logger provider with up to three processors: the
// redacting processor FIRST (always - see ledger.RedactingProcessor's doc
// for why every OTHER processor must run after it, never before), the
// ledger exporter (quack's built-in "collector" - appends every record to
// ledgerStore) when recording is enabled, and an OTLP log exporter when
// otlp_endpoint is set. The latter two are independently optional; a
// provider with only the redactor performs no externally-visible operation
// (see sdklog.NewLoggerProvider).
func initLogs(ctx context.Context, res *resource.Resource, cfg config.ObservabilityConfig, ledgerStore ledger.LedgerStore) (*sdklog.LoggerProvider, func(context.Context) error, error) {
	opts := []sdklog.LoggerProviderOption{
		sdklog.WithResource(res),
		sdklog.WithProcessor(ledger.NewRedactingProcessor()),
	}
	var shutdowns []func(context.Context) error

	if cfg.Recording.IsEnabled(cfg.Otel.IsEnabled()) {
		if ledgerStore == nil {
			// Forbidden: recording must never change run behavior. A store that
			// failed to resolve (bad config, unwritable root) degrades to "not
			// recording", not a startup failure.
			logf("observability.recording is enabled but no ledger store resolved; recording is disabled for this run")
		} else {
			proc := sdklog.NewSimpleProcessor(ledger.NewExporter(ledgerStore))
			opts = append(opts, sdklog.WithProcessor(proc))
		}
	}
	if cfg.Otel.OTLPEndpoint != "" {
		lexp, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(cfg.Otel.OTLPEndpoint))
		if err != nil {
			return nil, nil, fmt.Errorf("otelobs: otlp log exporter: %w", err)
		}
		bp := sdklog.NewBatchProcessor(lexp)
		opts = append(opts, sdklog.WithProcessor(bp))
		shutdowns = append(shutdowns, bp.Shutdown)
	}

	lp := sdklog.NewLoggerProvider(opts...)
	loggerProvider.Store(lp)
	shutdown := func(sctx context.Context) error {
		var firstErr error
		for _, fn := range append([]func(context.Context) error{lp.Shutdown}, shutdowns...) {
			if err := fn(sctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	return lp, shutdown, nil
}

// EmitLog records one gen_ai.* log event on scope's logger, stamping the
// execution coordinates carried on ctx (ledger.CoordsFromContext) as
// gen_ai.conversation.id / quack.node / quack.round - the ONE place these
// three attrs get attached, so no call site builds them by hand. body is
// the event's short human-readable summary ("" is fine; the substance lives
// in attrs).
func EmitLog(ctx context.Context, scope, body string, attrs ...otellog.KeyValue) {
	lg := Logger(scope)
	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	if body != "" {
		rec.SetBody(otellog.StringValue(body))
	}
	c := ledger.CoordsFromContext(ctx)
	rec.AddAttributes(otellog.String("gen_ai.semconv.version", GenAISemConvVersion))
	if c.ChatID != "" {
		rec.AddAttributes(otellog.String(GenAIConversationID, c.ChatID))
	}
	if c.Node != "" {
		rec.AddAttributes(otellog.String(quackNodeKey, c.Node))
	}
	if c.Round != "" {
		rec.AddAttributes(otellog.String(quackRoundKey, c.Round))
	}
	rec.AddAttributes(attrs...)
	lg.Emit(ctx, rec)
}

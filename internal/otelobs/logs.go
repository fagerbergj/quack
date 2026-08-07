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

// GenAISemConvVersion pins the semconv vocabulary for replay divergence checks.
const GenAISemConvVersion = "1.41.0"

// gen_ai.* attribute keys drawn from OTel's semconv; quack.* attrs have no semconv equivalent.
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

	// GenAIOperationPlan - "plan" has no registered semconv enum.
	GenAIOperationPlan = "plan"
	// GenAIOperationPlanRejected - the plan judge declined a proposed plan (#693);
	// the ledger record of a rejection reason that must never reach the user reply.
	GenAIOperationPlanRejected = "plan_rejected"

	quackNodeKey  = "quack.node"
	quackRoundKey = "quack.round"
)

// vars, not consts - attribute.Value has no constant form.
var (
	GenAIOperationChat        = semconv.GenAIOperationNameChat.Value.AsString()
	GenAIOperationExecuteTool = semconv.GenAIOperationNameExecuteTool.Value.AsString()
	GenAIOperationInvokeAgent = semconv.GenAIOperationNameInvokeAgent.Value.AsString()

	// GenAIProviderOpenAI is quack's one implemented inference provider kind.
	GenAIProviderOpenAI = semconv.GenAIProviderNameOpenAI.Value.AsString()
)

// loggerProvider mirrors tracer()'s lazy-global pattern; logs have no stable global slot.
var loggerProvider atomic.Pointer[sdklog.LoggerProvider]

// Logger returns a named logger; before Init runs this is a noop.Logger.
func Logger(name string) otellog.Logger {
	if lp := loggerProvider.Load(); lp != nil {
		return lp.Logger(name)
	}
	return noop.NewLoggerProvider().Logger(name)
}

// LoggingEnabled checks something is listening - avoids building expensive gen_ai event payloads.
func LoggingEnabled(scope string) bool {
	return Logger(scope).Enabled(context.Background(), otellog.EnabledParameters{})
}

// SetLoggerProviderForTesting overrides the process-wide logger provider (test-only).
func SetLoggerProviderForTesting(lp *sdklog.LoggerProvider) (restore func()) {
	prev := loggerProvider.Load()
	loggerProvider.Store(lp)
	return func() { loggerProvider.Store(prev) }
}

// initLogs builds the logger provider with redacting + optional ledger + OTLP processors.
func initLogs(ctx context.Context, res *resource.Resource, cfg config.ObservabilityConfig, ledgerStore ledger.LedgerStore) (*sdklog.LoggerProvider, func(context.Context) error, error) {
	opts := []sdklog.LoggerProviderOption{
		sdklog.WithResource(res),
		sdklog.WithProcessor(ledger.NewRedactingProcessor()),
	}
	var shutdowns []func(context.Context) error

	if cfg.Recording.IsEnabled(cfg.Otel.IsEnabled()) {
		if ledgerStore == nil {
			// recording must never change run behavior - degrades to not recording.
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

// EmitLog records one gen_ai.* log event, stamping execution coordinates from ctx.
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

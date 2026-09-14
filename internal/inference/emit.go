package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// inferenceScope names the logger every chat event is emitted through -
// internal/otelobs.Logger(scope) picks the instrumentation scope.
const inferenceScope = "quack.inference"

// emitChatEvent records one gen_ai.* "chat" log event for a completed model
// call - the full request and the FINAL assembled response (see the
// GenerateContent doc comment on why "final", not raw stream chunks). Marshal failures degrade a field to omitted, never abort the whole event - recording must never affect the run.
// chatRequestAttrs: the gen_ai request attributes (contents, tools, config knobs);
// returns them plus the system-instruction hash (prompt provenance).
func chatRequestAttrs(req *model.LLMRequest) ([]attribute.KeyValue, string) {
	var attrs []attribute.KeyValue
	if Version != "" {
		attrs = append(attrs, attribute.String(otelobs.QuackVersion, Version))
	}
	if v, ok := marshalAttr(req.Contents); ok {
		attrs = append(attrs, attribute.String(otelobs.GenAIInputMessages, v))
	}
	if names := toolNames(req.Tools); len(names) > 0 {
		if v, ok := marshalAttr(names); ok {
			attrs = append(attrs, attribute.String(otelobs.GenAIToolDefinitions, v))
		}
	}
	var sysHash string
	if req.Config != nil {
		if req.Config.SystemInstruction != nil {
			if v, ok := marshalAttr(req.Config.SystemInstruction); ok {
				attrs = append(attrs, attribute.String(otelobs.GenAISystemInstructions, v))
				sysHash = contentHash([]byte(v))
			}
		}
		if req.Config.Temperature != nil {
			attrs = append(attrs, attribute.Float64(otelobs.GenAIRequestTemperature, float64(*req.Config.Temperature)))
		}
		if req.Config.MaxOutputTokens != 0 {
			attrs = append(attrs, attribute.Int64(otelobs.GenAIRequestMaxTokens, int64(req.Config.MaxOutputTokens)))
		}
		if req.Config.Seed != nil {
			attrs = append(attrs, attribute.Int64(otelobs.GenAIRequestSeed, int64(*req.Config.Seed)))
		}
		if tc := req.Config.ThinkingConfig; tc != nil {
			attrs = append(attrs, attribute.String(otelobs.GenAIRequestReasoningEffort, string(tc.ThinkingLevel)))
		}
	}
	return attrs, sysHash
}

// chatProvenanceAttrs: prompt provenance - the coordinating agent name (the closest
// proxy for the bundle id at this layer) and the system-instruction hash.
func chatProvenanceAttrs(ctx context.Context, sysHash string) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	c := ledger.CoordsFromContext(ctx)
	if c.Agent != "" {
		attrs = append(attrs, attribute.String(otelobs.GenAIAgentName, c.Agent))
		attrs = append(attrs, attribute.String(otelobs.GenAIPromptName, c.Agent))
	}
	if c.BundleHash != "" {
		attrs = append(attrs, attribute.String(otelobs.QuackBundleHash, c.BundleHash))
	}
	if sysHash != "" {
		attrs = append(attrs, attribute.String(otelobs.GenAIPromptVersion, sysHash))
	}
	return attrs
}

// chatResponseAttrs: the gen_ai response attributes (content, model, finish reason,
// usage/cost - input excludes cached, matching the otel metric's convention).
func chatResponseAttrs(resp *model.LLMResponse, pricing *config.ModelPricing) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if resp == nil {
		return attrs
	}
	if v, ok := marshalAttr(resp.Content); ok {
		attrs = append(attrs, attribute.String(otelobs.GenAIOutputMessages, v))
	}
	if resp.ModelVersion != "" {
		attrs = append(attrs, attribute.String(otelobs.GenAIResponseModel, resp.ModelVersion))
	}
	if resp.FinishReason != "" {
		// semconv models finish_reasons as an array; quack carries a single candidate.
		attrs = append(attrs, attribute.Slice(otelobs.GenAIResponseFinishReasons, attribute.StringValue(string(resp.FinishReason))))
	}
	if u := resp.UsageMetadata; u != nil {
		// Cost accounting per node/round, and the parity a swapped-model eval re-run (#606) compares against.
		input, cached := splitPromptTokens(u)
		if input != 0 {
			attrs = append(attrs, attribute.Int64(otelobs.GenAIUsageInputTokens, input))
		}
		if cached != 0 {
			attrs = append(attrs, attribute.Int64(otelobs.GenAIUsageCachedTokens, cached))
		}
		if u.CandidatesTokenCount != 0 {
			attrs = append(attrs, attribute.Int64(otelobs.GenAIUsageOutputTokens, int64(u.CandidatesTokenCount)))
		}
		if pricing != nil {
			cost := float64(u.PromptTokenCount)/1e6*pricing.InputPerMTok + float64(u.CandidatesTokenCount+u.ThoughtsTokenCount)/1e6*pricing.OutputPerMTok
			attrs = append(attrs, attribute.Float64(otelobs.GenAIUsageCost, cost))
		}
	}
	return attrs
}

func emitChatEvent(ctx context.Context, name string, req *model.LLMRequest, resp *model.LLMResponse, callErr error, pricing *config.ModelPricing) {
	if !otelobs.LoggingEnabled(inferenceScope) {
		return // nothing listening - skip building a (potentially large) event nobody reads
	}
	attrs := []attribute.KeyValue{
		attribute.String(otelobs.GenAIOperationName, otelobs.GenAIOperationChat),
		attribute.String(otelobs.GenAIProviderName, otelobs.GenAIProviderOpenAI),
		attribute.String(otelobs.GenAIRequestModel, name),
	}
	reqAttrs, sysHash := chatRequestAttrs(req)
	attrs = append(attrs, reqAttrs...)
	attrs = append(attrs, chatProvenanceAttrs(ctx, sysHash)...)
	attrs = append(attrs, chatResponseAttrs(resp, pricing)...)
	if callErr != nil {
		attrs = append(attrs, attribute.String(otelobs.ErrorType, callErr.Error()))
	}
	otelobs.EmitLog(ctx, inferenceScope, "", attrs...)
}

// marshalAttr JSON-marshals v for a gen_ai attribute value; shared by the log
// path (emitChatEvent) and the span path (span.go) so both render the same
// bytes for the same field. false when there is nothing worth recording.
func marshalAttr(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 || string(b) == "null" {
		return "", false
	}
	return string(b), true
}

// toolNames extracts the sorted tool names offered on the request. req.Tools
// holds live tool.Tool instances (json:"-" on LLMRequest, deliberately not
// serializable), so a name list is what gen_ai.tool.definitions carries here - a bundle's full per-tool schema is already visible in gen_ai.input.messages' system instructions and the tool's own execute_tool events.
func toolNames(tools map[string]any) []string {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// contentHash is the prompt-version content hash: a short, stable digest of
// the system instruction bytes, so replay's divergence report can tell "the
// prompt changed" from "everything else did".
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

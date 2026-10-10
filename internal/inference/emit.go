package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

const inferenceScope = "quack.inference"

// chatRequestAttrs also returns the system-instruction hash for prompt provenance.
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
			// genai's enum is upper-case; the ledger and models.<id>.effort use lower-case.
			attrs = append(attrs, attribute.String(otelobs.GenAIRequestReasoningEffort, strings.ToLower(string(tc.ThinkingLevel))))
		}
	}
	return attrs, sysHash
}

// chatProvenanceAttrs uses the agent name as the closest proxy for the bundle id at this layer.
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
	if c.PromptSource != "" {
		attrs = append(attrs, attribute.String(otelobs.QuackPromptSource, c.PromptSource))
	}
	if c.PromptVersionID != "" {
		attrs = append(attrs, attribute.String(otelobs.QuackPromptVersionID, c.PromptVersionID))
	}
	if c.PromptArtifact != "" {
		attrs = append(attrs, attribute.String(otelobs.QuackPromptArtifact, c.PromptArtifact))
	}
	if len(c.Artifacts) > 0 {
		if v, ok := marshalAttr(c.Artifacts); ok {
			attrs = append(attrs, attribute.String(otelobs.QuackArtifacts, v))
		}
	}
	if len(c.Plugins) > 0 {
		if v, ok := marshalAttr(c.Plugins); ok {
			attrs = append(attrs, attribute.String(otelobs.QuackPlugins, v))
		}
	}
	if sysHash != "" {
		attrs = append(attrs, attribute.String(otelobs.GenAIPromptVersion, sysHash))
	}
	return attrs
}

// chatResponseAttrs reports input excluding cached tokens, matching the otel metric.
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
		if u.ThoughtsTokenCount != 0 {
			attrs = append(attrs, attribute.Int64(otelobs.GenAIUsageReasoningTokens, int64(u.ThoughtsTokenCount)))
		}
		if pricing != nil {
			cost := callCost(pricing, int64(u.PromptTokenCount), int64(u.CandidatesTokenCount+u.ThoughtsTokenCount))
			attrs = append(attrs, attribute.Float64(otelobs.GenAIUsageCost, cost))
		}
	}
	return attrs
}

// emitChatEvent logs the full request and final response; a marshal failure omits that field
// rather than aborting, since recording must not affect the run.
func emitChatEvent(ctx context.Context, name string, req *model.LLMRequest, resp *model.LLMResponse, callErr error, pricing *config.ModelPricing) {
	if !otelobs.LoggingEnabled(inferenceScope) {
		return // skip building a large event nobody reads
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

// marshalAttr is shared by the log and span paths so both render identical bytes; false means nothing to record.
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

// toolNames: req.Tools holds live, unserializable tools, so gen_ai.tool.definitions carries names only.
func toolNames(tools map[string]any) []string { return slices.Sorted(maps.Keys(tools)) }

// contentHash is a short digest of the system instruction, so a diff can tell a prompt change apart.
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

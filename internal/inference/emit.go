package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	otellog "go.opentelemetry.io/otel/log"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// inferenceScope names the logger every chat event is emitted through -
// internal/otelobs.Logger(scope) picks the instrumentation scope.
const inferenceScope = "quack.inference"

// emitChatEvent records one gen_ai.* "chat" log event for a completed model
// call - the full request and the FINAL assembled response (see the
// GenerateContent doc comment on why "final", not raw stream chunks).
// Marshal failures degrade a field to omitted, never abort the whole event -
// recording must never affect the run.
func emitChatEvent(ctx context.Context, name string, req *model.LLMRequest, resp *model.LLMResponse, callErr error) {
	if !otelobs.LoggingEnabled(inferenceScope) {
		return // nothing listening - skip building a (potentially large) event nobody reads
	}
	attrs := []otellog.KeyValue{
		otellog.String(otelobs.GenAIOperationName, otelobs.GenAIOperationChat),
		otellog.String(otelobs.GenAIProviderName, otelobs.GenAIProviderOpenAI),
		otellog.String(otelobs.GenAIRequestModel, name),
	}
	if b, err := json.Marshal(req.Contents); err == nil {
		attrs = append(attrs, otellog.String(otelobs.GenAIInputMessages, string(b)))
	}
	if names := toolNames(req.Tools); len(names) > 0 {
		if b, err := json.Marshal(names); err == nil {
			attrs = append(attrs, otellog.String(otelobs.GenAIToolDefinitions, string(b)))
		}
	}

	var sysHash string
	if req.Config != nil {
		if req.Config.SystemInstruction != nil {
			if b, err := json.Marshal(req.Config.SystemInstruction); err == nil {
				attrs = append(attrs, otellog.String(otelobs.GenAISystemInstructions, string(b)))
				sysHash = contentHash(b)
			}
		}
		if req.Config.Temperature != nil {
			attrs = append(attrs, otellog.Float64(otelobs.GenAIRequestTemperature, float64(*req.Config.Temperature)))
		}
		if req.Config.MaxOutputTokens != 0 {
			attrs = append(attrs, otellog.Int64(otelobs.GenAIRequestMaxTokens, int64(req.Config.MaxOutputTokens)))
		}
		if req.Config.Seed != nil {
			attrs = append(attrs, otellog.Int64(otelobs.GenAIRequestSeed, int64(*req.Config.Seed)))
		}
	}

	// Prompt provenance: bundle id + content hash. The bundle id isn't
	// visible at this layer (the agent's InstructionProvider already
	// resolved it into Config.SystemInstruction by the time it reaches a
	// model call) - the coordinating agent name (ledger.Coords, set by the
	// vetting gate) is the closest available proxy.
	c := ledger.CoordsFromContext(ctx)
	if c.Agent != "" {
		attrs = append(attrs, otellog.String(otelobs.GenAIAgentName, c.Agent))
		attrs = append(attrs, otellog.String(otelobs.GenAIPromptName, c.Agent))
	}
	if sysHash != "" {
		attrs = append(attrs, otellog.String(otelobs.GenAIPromptVersion, sysHash))
	}

	if resp != nil {
		if b, err := json.Marshal(resp.Content); err == nil {
			attrs = append(attrs, otellog.String(otelobs.GenAIOutputMessages, string(b)))
		}
		if resp.ModelVersion != "" {
			attrs = append(attrs, otellog.String(otelobs.GenAIResponseModel, resp.ModelVersion))
		}
	}
	if callErr != nil {
		attrs = append(attrs, otellog.String(otelobs.ErrorType, callErr.Error()))
	}

	otelobs.EmitLog(ctx, inferenceScope, "", attrs...)
}

// toolNames extracts the sorted tool names offered on the request. req.Tools
// holds live tool.Tool instances (json:"-" on LLMRequest, deliberately not
// serializable), so a name list is what gen_ai.tool.definitions carries here -
// a bundle's full per-tool schema is already visible in gen_ai.input.messages'
// system instructions and the tool's own execute_tool events.
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

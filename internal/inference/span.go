package inference

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// These have no OTel semconv form. observation.prompt.* are Langfuse v4's
// documented prompt-link keys; the older langfuse.prompt.* alias is not sent.
const (
	langfuseObservationInput         = "langfuse.observation.input"
	langfuseObservationOutput        = "langfuse.observation.output"
	langfuseObservationPromptName    = "langfuse.observation.prompt.name"
	langfuseObservationPromptVersion = "langfuse.observation.prompt.version"
)

// spanAttrCap matches internal/acp/turnspan.go's 8KB truncation.
const spanAttrCap = 8192

func capSpanAttr(s string) string {
	if len(s) <= spanAttrCap {
		return s
	}
	return s[:spanAttrCap] + "…[truncated]"
}

// redactedSpanAttr redacts like the log pipeline's RedactingProcessor, which spans never pass through.
func redactedSpanAttr(v any) (string, bool) {
	s, ok := marshalAttr(v)
	if !ok {
		return "", false
	}
	var decoded any
	if err := json.Unmarshal([]byte(s), &decoded); err == nil {
		if b, err := json.Marshal(ledger.Redact(decoded)); err == nil {
			s = string(b)
		}
	}
	return capSpanAttr(s), true
}

// correlationAttrs are keys, never content, so they bypass the capture-content gate.
func correlationAttrs(ctx context.Context) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	c := ledger.CoordsFromContext(ctx)
	if c.ChatID == "" {
		return attrs
	}
	attrs = append(attrs, attribute.String(otelobs.GenAIConversationID, c.ChatID))
	// Node ids are free plan text, so a verbose one could carry message content past the gate.
	if isSlug(c.Node) {
		attrs = append(attrs, attribute.String(otelobs.QuackNode, c.Node))
	}
	if c.Agent != "" {
		attrs = append(attrs, attribute.String(otelobs.GenAIAgentName, c.Agent))
	}
	// Any store-resolved prompt links to Langfuse, whatever the store's name.
	if c.PromptSource != "" && c.PromptSource != artifactsrc.StaticSource && c.PromptArtifact != "" && c.PromptVersionID != "" {
		attrs = append(attrs,
			attribute.String(langfuseObservationPromptName, c.PromptArtifact),
			attribute.String(langfuseObservationPromptVersion, c.PromptVersionID),
		)
	}
	return attrs
}

// setRequestSpanAttrs decorates ADK's own generate_content span. It must run before the first
// non-partial yield: ADK ends the span then, and SetAttributes on an ended span is a no-op.
func setRequestSpanAttrs(ctx context.Context, req *model.LLMRequest) {
	span := oteltrace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return // skip building a large payload nobody exports
	}
	// ADK never says which node ran the span; without node/agent a multi-node trace can't be
	// narrowed to the card the user clicked.
	attrs := correlationAttrs(ctx)
	if !otelobs.CaptureContentEnabled() {
		if len(attrs) > 0 {
			span.SetAttributes(attrs...)
		}
		return // content capture is opt-in
	}
	if v, ok := redactedSpanAttr(req.Contents); ok {
		attrs = append(attrs,
			attribute.String(otelobs.GenAIInputMessages, v),
			attribute.String(langfuseObservationInput, v),
		)
	}
	if names := toolNames(req.Tools); len(names) > 0 {
		if v, ok := redactedSpanAttr(names); ok {
			attrs = append(attrs, attribute.String(otelobs.GenAIToolDefinitions, v))
		}
	}
	if req.Config != nil && req.Config.SystemInstruction != nil {
		if v, ok := redactedSpanAttr(req.Config.SystemInstruction); ok {
			attrs = append(attrs, attribute.String(otelobs.GenAISystemInstructions, v))
		}
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}

// setResponseSpanAttrs must run before a non-partial response is yielded (see setRequestSpanAttrs).
func setResponseSpanAttrs(ctx context.Context, resp *model.LLMResponse) {
	span := oteltrace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	if !otelobs.CaptureContentEnabled() {
		return
	}
	if v, ok := redactedSpanAttr(resp.Content); ok {
		span.SetAttributes(
			attribute.String(otelobs.GenAIOutputMessages, v),
			attribute.String(langfuseObservationOutput, v),
		)
	}
}

// isSlug reports whether s is short and slug-shaped - the plan's own
// convention, and the only shape safe to export unredacted.
func isSlug(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

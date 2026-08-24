package inference

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// langfuse.observation.* have no OTel semconv form; Langfuse doesn't read
// gen_ai.input/output.messages yet (langfuse#12657), so content needs both.
const (
	langfuseObservationInput  = "langfuse.observation.input"
	langfuseObservationOutput = "langfuse.observation.output"
)

// spanAttrCap bounds gen_ai content span attribute values; matches
// internal/acp/turnspan.go's 8KB truncation convention.
const spanAttrCap = 8192

func capSpanAttr(s string) string {
	if len(s) <= spanAttrCap {
		return s
	}
	return s[:spanAttrCap] + "…[truncated]"
}

// redactedSpanAttr marshals v, redacts it the same way the log pipeline's
// RedactingProcessor does, and caps it. Span attributes never pass through
// that processor, so this is the only redaction a span attribute gets.
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

// setRequestSpanAttrs decorates ADK's own generate_content GENERATION span
// (never opens a competing one - the span in ctx already IS the active one)
// with request content. Must run before GenerateContent's inner loop yields
// a response: ADK ends this span synchronously on the first non-partial
// response, and SetAttributes on an ended span is a silent no-op.
func setRequestSpanAttrs(ctx context.Context, req *model.LLMRequest) {
	span := oteltrace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return // nothing exporting - skip building the (possibly large) payload
	}
	var attrs []attribute.KeyValue
	// Correlation keys, not content - these stay outside the gate below. ADK
	// names the span but never says which node ran it; without node/agent a
	// multi-node trace can't be narrowed to the card the user clicked.
	if c := ledger.CoordsFromContext(ctx); c.ChatID != "" {
		attrs = append(attrs, attribute.String(otelobs.GenAIConversationID, c.ChatID))
		if c.Node != "" {
			attrs = append(attrs, attribute.String(otelobs.QuackNode, c.Node))
		}
		if c.Agent != "" {
			attrs = append(attrs, attribute.String(otelobs.GenAIAgentName, c.Agent))
		}
	}
	if !otelobs.CaptureContentEnabled() {
		if len(attrs) > 0 {
			span.SetAttributes(attrs...)
		}
		return // opt-in only - see otelobs.CaptureContentEnabled doc comment
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

// setResponseSpanAttrs decorates the span with the final response content.
// Call this BEFORE forwarding a non-partial response to yield - see the
// ordering-trap comment on setRequestSpanAttrs.
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

package acp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	sdk "github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// errToolUnfinished marks a tool call still in flight when the round exited
// (cancel, idle timeout, crash) - the span exports with an error status
// instead of vanishing, which is exactly the one worth seeing on a wedged round.
var errToolUnfinished = errors.New("acp: round ended with the tool call still running")

// turnSpans emits one child span per ACP tool call under quack.acp.prompt,
// started and ended as the session updates arrive, so a long round exports
// telemetry WHILE it runs rather than in one burst at the end (#924).
//
// Tool call is the granularity: thought and message chunks arrive per token
// (the 1,845 chat_events of a single review), tool calls arrive in the tens to
// low hundreds. The spans carry no model-named attribute - that is what types
// a span as a Langfuse GENERATION and folds wall-clock into per-model
// aggregates (#927/#930) - and opencode's own model calls go straight to
// llm-swap with no trace context, so they are neither nested nor linked here.
type turnSpans struct {
	ctx   context.Context // the prompt span's context: these hang off it
	agent string
	open  map[string]oteltrace.Span
}

// newTurnSpans binds to promptCtx, the context carrying quack.acp.prompt.
func newTurnSpans(promptCtx context.Context, agent string) *turnSpans {
	return &turnSpans{ctx: promptCtx, agent: agent, open: map[string]oteltrace.Span{}}
}

// observe starts or ends a tool-call span for u. Called only from round's
// update loop, one goroutine, so the map needs no lock.
func (t *turnSpans) observe(u sdk.SessionUpdate) {
	switch {
	case u.ToolCall != nil:
		c := u.ToolCall
		t.start(string(c.ToolCallId), c.Kind, c.Title, c.RawInput)
		t.record(string(c.ToolCallId), nil, c.RawOutput, c.Content)
		if terminalStatus(c.Status) {
			t.finish(string(c.ToolCallId), c.Status)
		}
	case u.ToolCallUpdate != nil:
		up := u.ToolCallUpdate
		t.record(string(up.ToolCallId), up.RawInput, up.RawOutput, up.Content)
		if up.Status != nil && terminalStatus(*up.Status) {
			t.finish(string(up.ToolCallId), *up.Status)
		}
	}
}

// attrCap bounds gen_ai.tool.call.* attribute values; matches the pi-acp
// shim's 8KB truncation convention (tools/pi-acp/otel.mjs).
const attrCap = 8192

func capAttr(s string) string {
	if len(s) <= attrCap {
		return s
	}
	return s[:attrCap] + "…[truncated]"
}

// jsonAttr renders v for a span attribute; false when there is nothing to record.
func jsonAttr(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return "", false
	}
	return capAttr(string(b)), true
}

// record stamps tool input/output onto an open span as they arrive.
// Later updates overwrite - the terminal update carries the final values.
func (t *turnSpans) record(id string, rawInput, rawOutput any, content []sdk.ToolCallContent) {
	span, ok := t.open[id]
	if !ok {
		return
	}
	if !span.IsRecording() || !otelobs.CaptureContentEnabled() {
		return // opt-in only - tool args/output are message content (observability.otel.capture_content)
	}
	if v, ok := jsonAttr(rawInput); ok {
		span.SetAttributes(attribute.String(otelobs.GenAIToolCallArguments, v))
	}
	if v, ok := jsonAttr(rawOutput); ok {
		span.SetAttributes(attribute.String(otelobs.GenAIToolCallResult, v))
	} else if txt := toolContentText(content); txt != "" {
		span.SetAttributes(attribute.String(otelobs.GenAIToolCallResult, capAttr(txt)))
	}
}

// contentText flattens a tool call's content blocks to plain text; the
// fallback result when the agent sends no rawOutput.
func toolContentText(content []sdk.ToolCallContent) string {
	var b strings.Builder
	for _, c := range content {
		switch {
		case c.Content != nil && c.Content.Content.Text != nil:
			b.WriteString(c.Content.Content.Text.Text)
		case c.Diff != nil:
			b.WriteString(c.Diff.Path)
		}
		if b.Len() > attrCap {
			break // enough to fill the capped attribute
		}
	}
	return b.String()
}

func (t *turnSpans) start(id string, kind sdk.ToolKind, title string, rawInput any) {
	if _, dup := t.open[id]; id == "" || dup {
		return
	}
	name := string(kind)
	if name == "" {
		name = "other"
	}
	// Kind in the span name (a fixed protocol enum, so bounded cardinality)
	// makes a trace readable without opening every span.
	attrs := []attribute.KeyValue{
		attribute.String(otelobs.GenAIAgentName, t.agent),
		attribute.String(otelobs.GenAIToolCallID, id),
		attribute.String("tool_title", title), // human label from ACP's ToolCall.Title, distinct from the tool name
	}
	if otelobs.CaptureContentEnabled() {
		if v, ok := jsonAttr(rawInput); ok {
			attrs = append(attrs, attribute.String(otelobs.GenAIToolCallArguments, v))
		}
	}
	_, span := otelobs.Start(t.ctx, "acp.tool."+name, attrs...)
	t.open[id] = span
}

func (t *turnSpans) finish(id string, status sdk.ToolCallStatus) {
	span, ok := t.open[id]
	if !ok {
		return // create-on-update: no start seen, so no span to close
	}
	delete(t.open, id)
	var err error
	if status == sdk.ToolCallStatusFailed {
		err = errors.New("acp: tool call failed")
	}
	otelobs.End(span, err)
}

// closeAll ends every span still open. Safe to call twice.
func (t *turnSpans) closeAll() {
	for id, span := range t.open {
		delete(t.open, id)
		otelobs.End(span, errToolUnfinished)
	}
}

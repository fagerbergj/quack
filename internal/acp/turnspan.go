package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// errToolUnfinished marks a tool call still in flight when the round exited, so its span exports with
// an error status instead of vanishing: the one worth seeing on a wedged round.
var errToolUnfinished = errors.New("acp: round ended with the tool call still running")

// turnSpans emits one child span per tool call under quack.acp.prompt as updates arrive, so long rounds export
// mid-run. No model-named attribute: that would type it a Langfuse GENERATION and skew per-model aggregates.
type turnSpans struct {
	ctx   context.Context // the prompt span's context: these hang off it
	agent string
	open  map[string]oteltrace.Span
}

// newTurnSpans binds to promptCtx, the context carrying quack.acp.prompt.
func newTurnSpans(promptCtx context.Context, agent string) *turnSpans {
	return &turnSpans{ctx: promptCtx, agent: agent, open: map[string]oteltrace.Span{}}
}

// observe starts or ends a tool-call span for u; specs are u's translation, whose response names a failure's
// cause. Called only from round's update loop, one goroutine, so the map needs no lock.
func (t *turnSpans) observe(u sdk.SessionUpdate, specs []eventSpec) {
	switch {
	case u.ToolCall != nil:
		c := u.ToolCall
		t.start(string(c.ToolCallId), c.Kind, c.Title, c.RawInput, c.Meta)
		t.record(string(c.ToolCallId), nil, c.RawOutput, c.Content)
		if terminalStatus(c.Status) {
			t.finish(string(c.ToolCallId), c.Status, specs)
		}
	case u.ToolCallUpdate != nil:
		up := u.ToolCallUpdate
		t.record(string(up.ToolCallId), up.RawInput, up.RawOutput, up.Content)
		if up.Status != nil && terminalStatus(*up.Status) {
			t.finish(string(up.ToolCallId), *up.Status, specs)
		}
	}
}

// attrCap bounds gen_ai.tool.call.* attribute values; matches the pi-acp
// shim's 8KB truncation convention (tools/pi-acp/otel.mjs).
const attrCap = 8192

// jsonAttr renders v for a span attribute; false when there is nothing to record.
func jsonAttr(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return "", false
	}
	return bound(string(b), attrCap), true
}

// record stamps tool input/output onto an open span as they arrive; later updates overwrite. Arguments
// ride ungated, as on ADK's native execute_tool spans, so a failed call shows what it was asked.
func (t *turnSpans) record(id string, rawInput, rawOutput any, content []sdk.ToolCallContent) {
	span, ok := t.open[id]
	if !ok || !span.IsRecording() {
		return
	}
	if v, ok := jsonAttr(rawInput); ok {
		span.SetAttributes(attribute.String(otelobs.GenAIToolCallArguments, v))
	}
	if !otelobs.CaptureContentEnabled() {
		return // opt-in only - tool output is message content (observability.otel.capture_content)
	}
	if v, ok := jsonAttr(rawOutput); ok {
		span.SetAttributes(attribute.String(otelobs.GenAIToolCallResult, v))
	} else if txt := toolContentText(content); txt != "" {
		span.SetAttributes(attribute.String(otelobs.GenAIToolCallResult, bound(txt, attrCap)))
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

func (t *turnSpans) start(id string, kind sdk.ToolKind, title string, rawInput any, meta map[string]any) {
	if _, dup := t.open[id]; id == "" || dup {
		return
	}
	// A bridged MCP call's span is named after the real tool (bounded set). An unresolved kind stays "other", not
	// the title: a span name is a cardinality dimension; the title rides the tool_title attribute.
	name := string(kind)
	if mcpName, ok := mcpIdentity(meta, title); ok {
		name = mcpName
	} else if name == "" || kind == sdk.ToolKindOther {
		name = "other"
	}
	// Kind in the span name (a fixed protocol enum, plus quack's own bounded
	// MCP tool set) makes a trace readable without opening every span.
	attrs := []attribute.KeyValue{
		attribute.String(otelobs.GenAIAgentName, t.agent),
		attribute.String(otelobs.GenAIToolCallID, id),
		attribute.String("tool_title", title), // human label from ACP's ToolCall.Title, distinct from the tool name
	}
	if v, ok := jsonAttr(rawInput); ok {
		attrs = append(attrs, attribute.String(otelobs.GenAIToolCallArguments, v))
	}
	_, span := otelobs.Start(t.ctx, "acp.tool."+name, attrs...)
	t.open[id] = span
}

func (t *turnSpans) finish(id string, status sdk.ToolCallStatus, specs []eventSpec) {
	span, ok := t.open[id]
	if !ok {
		return // create-on-update: no start seen, so no span to close
	}
	delete(t.open, id)
	var err error
	if status == sdk.ToolCallStatusFailed {
		err = fmt.Errorf("acp: tool call failed: %s", toolError(specs, id))
	}
	otelobs.End(span, err)
}

// toolError is call id's error text in specs, already bounded by toolResponse; "" when none carries it.
func toolError(specs []eventSpec, id string) string {
	for _, s := range specs {
		for _, p := range s.parts {
			if r := p.FunctionResponse; r != nil && r.ID == id {
				msg, _ := r.Response["error"].(string)
				return msg
			}
		}
	}
	return ""
}

// closeAll ends every span still open. Safe to call twice.
func (t *turnSpans) closeAll() {
	for id, span := range t.open {
		delete(t.open, id)
		otelobs.End(span, errToolUnfinished)
	}
}

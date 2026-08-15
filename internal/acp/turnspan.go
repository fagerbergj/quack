package acp

import (
	"context"
	"errors"

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
		t.start(string(c.ToolCallId), c.Kind, c.Title)
		if terminalStatus(c.Status) {
			t.finish(string(c.ToolCallId), c.Status)
		}
	case u.ToolCallUpdate != nil:
		up := u.ToolCallUpdate
		if up.Status != nil && terminalStatus(*up.Status) {
			t.finish(string(up.ToolCallId), *up.Status)
		}
	}
}

func (t *turnSpans) start(id string, kind sdk.ToolKind, title string) {
	if _, dup := t.open[id]; id == "" || dup {
		return
	}
	name := string(kind)
	if name == "" {
		name = "other"
	}
	// Kind in the span name (a fixed protocol enum, so bounded cardinality)
	// makes a trace readable without opening every span.
	_, span := otelobs.Start(t.ctx, "acp.tool."+name,
		attribute.String("agent", t.agent),
		attribute.String("tool_call_id", id),
		attribute.String("tool_title", title))
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

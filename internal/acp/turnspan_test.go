package acp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"
)

// withTestTracer installs an in-memory span recorder as the global tracer
// provider for the test's duration and returns it for assertions.
func withTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exp
}

func spanByName(t *testing.T, exp *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	var names []string
	for _, s := range exp.GetSpans() {
		names = append(names, s.Name)
	}
	t.Fatalf("span %q was never recorded; got %v", name, names)
	return tracetest.SpanStub{}
}

// withContentCapture flips otelobs' shared capture-content flag for the
// duration of one test, restoring the previous value on cleanup.
func withContentCapture(t *testing.T, enabled bool) {
	t.Helper()
	prev := otelobs.CaptureContentEnabled()
	otelobs.SetCaptureContent(enabled)
	t.Cleanup(func() { otelobs.SetCaptureContent(prev) })
}

func attrsOf(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// TestRound_ToolSpansEndInsideTheRound is #924: an ACP round that runs for two
// hours must not export its first span two hours in. The per-tool-call child
// spans end as their session updates are handled, so they flush while the
// round is still running - which the exporter records as an end time strictly
// earlier than the prompt and round spans that enclose them.
func TestRound_ToolSpansEndInsideTheRound(t *testing.T) {
	exp := withTestTracer(t)
	a := testAgent(t, "happy")
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "c1", Node: "n1", Agent: "code-implementer", User: "u1"})
	if err := a.round(ctx, t.TempDir(), "", nil, workspace.Caps{}, "add the feature", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}

	tool := spanByName(t, exp, "quack.acp.tool.execute")
	prompt := spanByName(t, exp, "quack.acp.prompt")
	round := spanByName(t, exp, "quack.acp.round")

	if !tool.EndTime.Before(prompt.EndTime) || !tool.EndTime.Before(round.EndTime) {
		t.Errorf("tool span ended at %v, prompt %v, round %v - it must close inside the round, not with it",
			tool.EndTime, prompt.EndTime, round.EndTime)
	}
	if tool.Parent.SpanID() != prompt.SpanContext.SpanID() {
		t.Errorf("tool span parent = %v, want the prompt span %v", tool.Parent.SpanID(), prompt.SpanContext.SpanID())
	}

	attrs := attrsOf(tool)
	if got := attrs[otelobs.GenAIConversationID]; got != "c1" {
		t.Errorf("tool span %s = %q, want c1 - it must land in the same Langfuse session as the rest of the run", otelobs.GenAIConversationID, got)
	}
	if got := attrs["tool_title"]; got != "go test ./..." {
		t.Errorf("tool span tool_title = %q, want the tool call's title", got)
	}
	// Any model-named attribute types the span as a Langfuse GENERATION and
	// drops wall-clock into every per-model cost aggregate (#927/#930). These
	// spans wrap a tool call, not a model call.
	for _, k := range []string{"model", otelobs.GenAIRequestModel, otelobs.GenAIResponseModel, "llm.model_name", otelobs.QuackModel} {
		if v, ok := attrs[k]; ok {
			t.Errorf("tool span carries %s=%q - that types it as a GENERATION", k, v)
		}
	}
}

// TestTurnSpans_UnfinishedToolCallStillExports covers the wedged round: a tool
// call that never reaches a terminal status still gets a span, ended with an
// error when the round exits, so a stall is visible without querying Postgres.
func TestTurnSpans_UnfinishedToolCallStillExports(t *testing.T) {
	exp := withTestTracer(t)
	turns := newTurnSpans(context.Background(), "code-reviewer")

	turns.observe(sdk.StartToolCall("t1", "go test ./...", sdk.WithStartKind(sdk.ToolKindExecute)))
	turns.observe(sdk.UpdateAgentThoughtText("still thinking")) // no span of its own
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("%d spans ended already, want 0 while the tool call is in flight", n)
	}

	turns.closeAll()
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "quack.acp.tool.execute" {
		t.Fatalf("spans = %+v, want one quack.acp.tool.execute", spans)
	}
	if spans[0].Status.Description != errToolUnfinished.Error() {
		t.Errorf("status = %q, want %q", spans[0].Status.Description, errToolUnfinished)
	}
	turns.closeAll() // idempotent
	if n := len(exp.GetSpans()); n != 1 {
		t.Errorf("second closeAll produced %d spans, want 1", n)
	}
}

// TestTurnSpans_ToolCallDetailAttributes: the span carries the actual call -
// arguments from rawInput, result from the terminal update - so a runaway
// tool call is diagnosable from Langfuse without querying chat_events.
func TestTurnSpans_ToolCallDetailAttributes(t *testing.T) {
	withContentCapture(t, true)
	exp := withTestTracer(t)
	turns := newTurnSpans(context.Background(), "code-reviewer")

	turns.observe(sdk.StartToolCall("t1", "go test ./...",
		sdk.WithStartKind(sdk.ToolKindExecute),
		sdk.WithStartRawInput(map[string]any{"command": "go test ./..."})))
	turns.observe(sdk.UpdateToolCall("t1",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateRawOutput(map[string]any{"output": "ok\tquack\t0.1s"})))

	attrs := attrsOf(spanByName(t, exp, "quack.acp.tool.execute"))
	if got := attrs[otelobs.GenAIToolCallArguments]; got != `{"command":"go test ./..."}` {
		t.Errorf("%s = %q, want the rawInput JSON", otelobs.GenAIToolCallArguments, got)
	}
	if got := attrs[otelobs.GenAIToolCallResult]; got != "{\"output\":\"ok\\tquack\\t0.1s\"}" {
		t.Errorf("%s = %q, want the rawOutput JSON", otelobs.GenAIToolCallResult, got)
	}
}

// TestTurnSpans_ToolCallDetailAbsentByDefault: with capture off (the deploy
// default), tool call arguments/output never reach the span, even though it
// is recording - ACP does most of the tool-calling in this codebase, so this
// is the invariant that actually matters.
func TestTurnSpans_ToolCallDetailAbsentByDefault(t *testing.T) {
	exp := withTestTracer(t)
	turns := newTurnSpans(context.Background(), "code-reviewer")

	turns.observe(sdk.StartToolCall("t1", "go test ./...",
		sdk.WithStartKind(sdk.ToolKindExecute),
		sdk.WithStartRawInput(map[string]any{"command": "go test ./..."})))
	turns.observe(sdk.UpdateToolCall("t1",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateRawOutput(map[string]any{"output": "ok\tquack\t0.1s"})))

	attrs := attrsOf(spanByName(t, exp, "quack.acp.tool.execute"))
	for _, k := range []string{otelobs.GenAIToolCallArguments, otelobs.GenAIToolCallResult} {
		if v, ok := attrs[k]; ok {
			t.Errorf("attribute %q = %q present with content capture off, want absent", k, v)
		}
	}
}

// TestTurnSpans_ContentFallbackAndTruncation: with no rawOutput the result
// comes from the content text blocks, and oversized values are capped at
// attrCap with the shim's truncation marker.
func TestTurnSpans_ContentFallbackAndTruncation(t *testing.T) {
	withContentCapture(t, true)
	exp := withTestTracer(t)
	turns := newTurnSpans(context.Background(), "code-reviewer")

	big := strings.Repeat("x", attrCap+100)
	turns.observe(sdk.StartToolCall("t1", "cat big.log",
		sdk.WithStartKind(sdk.ToolKindExecute),
		sdk.WithStartRawInput(map[string]any{"command": big})))
	turns.observe(sdk.UpdateToolCall("t1",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateContent([]sdk.ToolCallContent{sdk.ToolContent(sdk.TextBlock(big))})))

	attrs := attrsOf(spanByName(t, exp, "quack.acp.tool.execute"))
	for _, k := range []string{otelobs.GenAIToolCallArguments, otelobs.GenAIToolCallResult} {
		v := attrs[k]
		if !strings.HasSuffix(v, "…[truncated]") {
			t.Errorf("%s does not end with the truncation marker: ...%q", k, v[len(v)-20:])
		}
		if len(v) > attrCap+len("…[truncated]") {
			t.Errorf("%s is %d bytes, want at most attrCap+marker", k, len(v))
		}
	}
}

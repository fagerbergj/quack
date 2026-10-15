package acp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/workspace"
)

func TestTeeBufferLinesSkipsBlankAndInvalidJSON(t *testing.T) {
	tb := &teeBuffer{}
	tb.Write([]byte(`{"jsonrpc":"2.0","method":"initialize"}` + "\n"))
	tb.Write([]byte("\n")) // blank line, common in ndjson framing
	tb.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	tb.Write([]byte(`not json`))

	lines := tb.lines()
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (blank + invalid dropped): %s", len(lines), lines)
	}
	var m map[string]any
	if err := json.Unmarshal(lines[0], &m); err != nil || m["method"] != "initialize" {
		t.Errorf("line 0 = %s, want the initialize message", lines[0])
	}
}

func TestTeeBufferCapsAtMaxBytes(t *testing.T) {
	tb := &teeBuffer{}
	big := make([]byte, maxTeeBytes+1000)
	for i := range big {
		big[i] = 'x'
	}
	n, err := tb.Write(big)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// A tee must never report a short write to its caller - that would break
	// the real io.Writer (stdin) it's paired with via io.MultiWriter.
	if n != len(big) {
		t.Errorf("Write returned n=%d, want %d (tee must never short-report)", n, len(big))
	}
	if tb.buf.Len() != maxTeeBytes {
		t.Errorf("captured %d bytes, want capped at %d", tb.buf.Len(), maxTeeBytes)
	}
}

func TestEmitInvokeAgent_ProducesWellFormedEvent(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	sent := &teeBuffer{}
	sent.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt"}` + "\n"))
	received := &teeBuffer{}
	received.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}` + "\n"))

	emitInvokeAgent(context.Background(), "code-implementer", sent, received, nil, nil, nil)

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := map[string]attribute.Value{}
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if got := attrs["gen_ai.operation.name"].AsString(); got != "invoke_agent" {
		t.Errorf("gen_ai.operation.name = %q, want invoke_agent", got)
	}
	if got := attrs["gen_ai.agent.name"].AsString(); got != "code-implementer" {
		t.Errorf("gen_ai.agent.name = %q, want code-implementer", got)
	}
	var in []json.RawMessage
	if err := json.Unmarshal([]byte(attrs["gen_ai.input.messages"].AsString()), &in); err != nil || len(in) != 1 {
		t.Errorf("gen_ai.input.messages = %v (err %v), want 1 sent frame", attrs["gen_ai.input.messages"], err)
	}
	var out []json.RawMessage
	if err := json.Unmarshal([]byte(attrs["gen_ai.output.messages"].AsString()), &out); err != nil || len(out) != 1 {
		t.Errorf("gen_ai.output.messages = %v (err %v), want 1 received frame", attrs["gen_ai.output.messages"], err)
	}
	if _, ok := attrs["error.type"]; ok {
		t.Error("error.type present on a successful round")
	}
}

// TestEmitInvokeAgent_RecordsPlugins: quack.plugins carries the round's plugin provenance as the JSON array
// exporter.go maps into AgentInvokePayload.Plugins.
func TestEmitInvokeAgent_RecordsPlugins(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	emitInvokeAgent(context.Background(), "code-implementer", &teeBuffer{}, &teeBuffer{}, nil,
		[]ledger.PluginRef{{Name: "dotagents", SHA: "abc123"}}, nil)

	attrs := map[string]attribute.Value{}
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if got := attrs["quack.plugins"].AsString(); got != `[{"name":"dotagents","sha":"abc123"}]` {
		t.Errorf("quack.plugins = %q, want the literal wire shape", got)
	}
}

// TestEmitInvokeAgent_RecordsArtifacts: quack.artifacts carries the round's resolved-artifact provenance
// as the JSON array exporter.go maps into AgentInvokePayload.Artifacts.
func TestEmitInvokeAgent_RecordsArtifacts(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	emitInvokeAgent(context.Background(), "code-implementer", &teeBuffer{}, &teeBuffer{}, nil,
		nil, []ledger.ArtifactRef{{Name: "system/acp.environment", Source: "static", VersionID: "abc123"}})

	attrs := map[string]attribute.Value{}
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if got := attrs["quack.artifacts"].AsString(); got != `[{"name":"system/acp.environment","source":"static","version_id":"abc123"}]` {
		t.Errorf("quack.artifacts = %q, want the literal wire shape", got)
	}
}

func TestEmitInvokeAgent_RecordsErrorType(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	emitInvokeAgent(context.Background(), "code-reviewer", &teeBuffer{}, &teeBuffer{}, errors.New("acp: prompt: boom"), nil, nil)

	attrs := map[string]attribute.Value{}
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if attrs["error.type"].AsString() != "acp: prompt: boom" {
		t.Errorf("error.type = %q, want the round error", attrs["error.type"].AsString())
	}
}

// captureExporter records every emitted record - mirrors internal/inference's
// test helper of the same name (unexported, package-local by design).
type captureExporter struct{ records []sdklog.Record }

func (c *captureExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *captureExporter) Shutdown(context.Context) error   { return nil }
func (c *captureExporter) ForceFlush(context.Context) error { return nil }

// TestRound_EachToolCallIsAToolCall: every finished ACP tool call becomes one tool.call row on the round's stream
// and one quack.tool.calls count, a failure carrying its error; its span names the cause and keeps the arguments.
func TestRound_EachToolCallIsAToolCall(t *testing.T) {
	store := ledgertest.NewMemStore()
	t.Cleanup(otelobs.SetLoggerProviderForTesting(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledger.NewExporter(store))))))
	exp := withTestTracer(t)
	reader := newUsageTestMeter(t)
	withContentCapture(t, false)
	a := testAgent(t, "tool-fail")
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "c1", Node: "code-reviewer-1", Round: "worker-r0"})
	if err := a.round(ctx, t.TempDir(), "", workspace.Caps{}, "review", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}

	const reason = "notes: item 1 is 221 characters, max 200"
	entries, _ := store.ReadEntries(context.Background(), "c1", 0)
	var rows []ledger.ToolCallPayload
	for _, e := range entries {
		if e.Kind != ledger.KindToolCall {
			continue
		}
		if e.NodeID != "code-reviewer-1" || e.Round != "worker-r0" || e.Agent != "code-implementer" {
			t.Errorf("tool.call on %s/%s/%s, want code-reviewer-1/code-implementer/worker-r0", e.NodeID, e.Agent, e.Round)
		}
		var p ledger.ToolCallPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, p)
	}
	want := []ledger.ToolCallPayload{
		{Name: "stage_review", Type: "function", Args: `{"event":"approve"}`, Error: reason},
		{Name: "run_command", Type: "function", Args: `{"command":"ls"}`, Result: `{"exit_code":0,"output":"go.mod"}`},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("tool.call rows = %+v\nwant %+v", rows, want)
	}

	span := spanByName(t, exp, "quack.acp.tool.stage_review")
	if got := span.Status.Description; got != "acp: tool call failed: "+reason {
		t.Errorf("span status = %q, want the tool's own error", got)
	}
	if got := attrsOf(span)[otelobs.GenAIToolCallArguments]; got != `{"event":"approve"}` {
		t.Errorf("span arguments = %q, want the call's input even with content capture off", got)
	}

	met, ok := collectMetric(t, reader, "quack.tool.calls")
	if !ok {
		t.Fatal("quack.tool.calls never recorded")
	}
	counts := map[string]int64{}
	for _, dp := range met.Data.(metricdata.Sum[int64]).DataPoints {
		r, _ := dp.Attributes.Value("rejected")
		counts[attrVal(dp.Attributes, "tool")+"/"+r.String()] += dp.Value
	}
	if wantCounts := map[string]int64{"stage_review/true": 1, "run_command/false": 1}; !reflect.DeepEqual(counts, wantCounts) {
		t.Errorf("quack.tool.calls = %v, want %v", counts, wantCounts)
	}
}

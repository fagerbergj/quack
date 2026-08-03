package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/otelobs"
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

	emitInvokeAgent(context.Background(), "code-implementer", sent, received, nil)

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := map[string]otellog.Value{}
	capExp.records[0].WalkAttributes(func(kv otellog.KeyValue) bool {
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

func TestEmitInvokeAgent_RecordsErrorType(t *testing.T) {
	capExp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	emitInvokeAgent(context.Background(), "code-reviewer", &teeBuffer{}, &teeBuffer{}, errors.New("acp: prompt: boom"))

	attrs := map[string]otellog.Value{}
	capExp.records[0].WalkAttributes(func(kv otellog.KeyValue) bool {
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

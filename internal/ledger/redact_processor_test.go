package ledger

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// captureProcExporter is a minimal sdklog.Exporter that records whatever it
// receives - standing in for the ledger exporter and (separately) an OTLP
// exporter, so a test can assert BOTH see the same, already-redacted data.
type captureProcExporter struct{ records []sdklog.Record }

func (c *captureProcExporter) Export(_ context.Context, records []sdklog.Record) error {
	for _, r := range records {
		c.records = append(c.records, r.Clone())
	}
	return nil
}
func (c *captureProcExporter) Shutdown(context.Context) error   { return nil }
func (c *captureProcExporter) ForceFlush(context.Context) error { return nil }

func attrString(t *testing.T, r sdklog.Record, key string) string {
	t.Helper()
	var got string
	found := false
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == key {
			got = kv.Value.AsString()
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("attribute %q not found", key)
	}
	return got
}

// TestRedactingProcessorProtectsEveryDownstreamProcessor is the blocking
// finding's regression test: a LoggerProvider wired like production - the
// redacting processor first, then TWO independent exporters (the ledger
// path and an OTLP stand-in) - must hand BOTH exporters already-redacted
// data. Before this processor existed, only the ledger exporter's own
// Export-time Redact call protected its path; a second processor (added
// whenever otlp_endpoint is configured) saw the raw record.
func TestRedactingProcessorProtectsEveryDownstreamProcessor(t *testing.T) {
	ledgerExp := &captureProcExporter{}
	otlpStandIn := &captureProcExporter{}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(NewRedactingProcessor()),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(ledgerExp)),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(otlpStandIn)),
	)
	logger := lp.Logger("test")

	var rec otellog.Record
	rec.AddAttributes(
		attribute.String("authorization", "Bearer sekret"),
		attribute.String("gen_ai.tool.call.arguments", `{"headers":{"api_key":"abc123"},"url":"https://x"}`),
		attribute.String("gen_ai.operation.name", "execute_tool"), // not a secret - must survive untouched
	)
	logger.Emit(context.Background(), rec)

	for name, exp := range map[string]*captureProcExporter{"ledger": ledgerExp, "otlp-stand-in": otlpStandIn} {
		if len(exp.records) != 1 {
			t.Fatalf("%s exporter got %d records, want 1", name, len(exp.records))
		}
		got := exp.records[0]
		if v := attrString(t, got, "authorization"); v != redactedValue {
			t.Errorf("%s: authorization = %q, want redacted", name, v)
		}
		args := attrString(t, got, "gen_ai.tool.call.arguments")
		if args == `{"headers":{"api_key":"abc123"},"url":"https://x"}` {
			t.Errorf("%s: gen_ai.tool.call.arguments reached the exporter UNREDACTED: %s", name, args)
		}
		if want := `{"headers":{"api_key":"[REDACTED]"},"url":"https://x"}`; args != want {
			t.Errorf("%s: gen_ai.tool.call.arguments = %s, want %s", name, args, want)
		}
		if v := attrString(t, got, "gen_ai.operation.name"); v != "execute_tool" {
			t.Errorf("%s: gen_ai.operation.name = %q, want untouched execute_tool", name, v)
		}
	}
}

// TestRedactingProcessorIsIdempotent guards the "double-redaction must stay
// harmless" property the ledger exporter's OWN redaction relies on: running
// an already-redacted record through the processor again changes nothing.
func TestRedactingProcessorIsIdempotent(t *testing.T) {
	exp := &captureProcExporter{}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(NewRedactingProcessor()),
		sdklog.WithProcessor(NewRedactingProcessor()), // twice, deliberately
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)),
	)
	logger := lp.Logger("test")

	var rec otellog.Record
	rec.AddAttributes(attribute.String("authorization", "Bearer sekret"))
	logger.Emit(context.Background(), rec)

	if len(exp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(exp.records))
	}
	if v := attrString(t, exp.records[0], "authorization"); v != redactedValue {
		t.Errorf("authorization = %q, want redacted (unchanged by a second pass)", v)
	}
}

package ledger

import (
	"context"
	"strings"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// RedactingProcessor is a no-op sdklog.Processor whose only job is to
// redact a record's attributes in place before any OTHER processor observes
// it. Register it FIRST: the SDK invokes processors sequentially in
// WithProcessor order, and a processor may synchronously mutate the record
// so the change is visible to the next one - so every processor on the
// provider only ever sees redacted data. Without this, a processor added
// alongside the ledger exporter would export raw, unredacted records past
// the ledger's own redaction, which only guards the ledger's OWN write path.
//
// The ledger exporter keeps its own redaction too (defense in depth); Redact
// is idempotent, so an already-redacted record is unchanged either way.
type RedactingProcessor struct{}

// NewRedactingProcessor returns a RedactingProcessor.
func NewRedactingProcessor() *RedactingProcessor { return &RedactingProcessor{} }

var _ sdklog.Processor = (*RedactingProcessor)(nil)

func (*RedactingProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	n := record.AttributesLen()
	if n == 0 {
		return nil
	}
	kvs := make([]otellog.KeyValue, 0, n)
	record.WalkAttributes(func(kv otellog.KeyValue) bool {
		kvs = append(kvs, redactKeyValue(kv))
		return true
	})
	record.SetAttributes(kvs...) // replaces, not appends - see Record.SetAttributes
	return nil
}

func (*RedactingProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (*RedactingProcessor) Shutdown(context.Context) error                         { return nil }
func (*RedactingProcessor) ForceFlush(context.Context) error                       { return nil }

// redactKeyValue redacts one attribute: a credential-NAMED key is replaced
// outright; a string VALUE is probed as JSON (redactJSONString, redact.go) -
// the shape every emission seam (inference/tools/acp) actually uses for a
// complex payload. Any other value kind (int64/float64/bool/slice/map)
// passes through unchanged - none of quack's seams put a credential in a
// non-string attribute value.
func redactKeyValue(kv otellog.KeyValue) otellog.KeyValue {
	if redactedKeys[strings.ToLower(kv.Key)] {
		return otellog.String(kv.Key, redactedValue)
	}
	if kv.Value.Kind() == otellog.KindString {
		return otellog.String(kv.Key, redactJSONString(kv.Value.AsString()))
	}
	return kv
}

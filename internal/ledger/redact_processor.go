package ledger

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// RedactingProcessor redacts record attributes before other processors see them.
// Registered FIRST so all downstream processors see redacted data.
type RedactingProcessor struct{}

// NewRedactingProcessor returns a RedactingProcessor.
func NewRedactingProcessor() *RedactingProcessor { return &RedactingProcessor{} }

var _ sdklog.Processor = (*RedactingProcessor)(nil)

func (*RedactingProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	n := record.AttributesLen()
	if n == 0 {
		return nil
	}
	kvs := make([]attribute.KeyValue, 0, n)
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		kvs = append(kvs, redactKeyValue(kv))
		return true
	})
	record.SetAttributes(kvs...) // replaces, not appends - see Record.SetAttributes
	return nil
}

func (*RedactingProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (*RedactingProcessor) Shutdown(context.Context) error                         { return nil }
func (*RedactingProcessor) ForceFlush(context.Context) error                       { return nil }

// redactKeyValue redacts credential-named keys and string values probed as JSON.
func redactKeyValue(kv attribute.KeyValue) attribute.KeyValue {
	if redactedKeys[strings.ToLower(string(kv.Key))] {
		return attribute.String(string(kv.Key), redactedValue)
	}
	if kv.Value.Type() == attribute.STRING {
		return attribute.String(string(kv.Key), redactJSONString(kv.Value.AsString()))
	}
	return kv
}

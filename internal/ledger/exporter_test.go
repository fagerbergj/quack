package ledger

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// memStore is a tiny in-memory LedgerStore for testing the Exporter without
// touching disk.
type memStore struct {
	lines map[string][][]byte
}

func newMemStore() *memStore { return &memStore{lines: map[string][][]byte{}} }

func (m *memStore) Append(_ context.Context, sessionID string, entry []byte) error {
	m.lines[sessionID] = append(m.lines[sessionID], append([]byte{}, entry...))
	return nil
}
func (m *memStore) ReadStream(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (m *memStore) List(context.Context) ([]SessionRef, error)                { return nil, nil }
func (m *memStore) Delete(context.Context, string) error                      { return nil }

var _ LedgerStore = (*memStore)(nil)

func TestExporterAppendsRedactedJSONKeyedByConversation(t *testing.T) {
	store := newMemStore()
	exp := NewExporter(store)

	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	logger := provider.Logger("test")

	var rec otellog.Record
	rec.SetBody(otellog.StringValue("chat"))
	rec.AddAttributes(
		otellog.String("gen_ai.conversation.id", "chat-42"),
		otellog.String("gen_ai.operation.name", "chat"),
		otellog.String("authorization", "Bearer secret"),
	)
	logger.Emit(context.Background(), rec)

	lines, ok := store.lines["chat-42"]
	if !ok || len(lines) != 1 {
		t.Fatalf("got sessions %v, want exactly one line under chat-42", store.lines)
	}

	var got line
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("emitted line is not valid JSON: %v (%s)", err, lines[0])
	}
	if got.Attrs["gen_ai.operation.name"] != "chat" {
		t.Errorf("operation.name = %v, want chat", got.Attrs["gen_ai.operation.name"])
	}
	if got.Attrs["authorization"] != redactedValue {
		t.Errorf("authorization = %v, want redacted", got.Attrs["authorization"])
	}
	if got.Body != "chat" {
		t.Errorf("body = %v, want chat", got.Body)
	}
}

func TestExporterDisabledStoreIsNoop(t *testing.T) {
	exp := NewExporter(nil)
	if err := exp.Export(context.Background(), nil); err != nil {
		t.Fatalf("Export with nil store returned an error: %v", err)
	}
}

func TestExporterUnscopedFallsBackToKnownSession(t *testing.T) {
	store := newMemStore()
	exp := NewExporter(store)
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	logger := provider.Logger("test")

	var rec otellog.Record
	rec.SetBody(otellog.StringValue("no conversation id"))
	logger.Emit(context.Background(), rec)

	if _, ok := store.lines[unscopedSession]; !ok {
		t.Fatalf("got sessions %v, want a fallback entry under %q", store.lines, unscopedSession)
	}
}

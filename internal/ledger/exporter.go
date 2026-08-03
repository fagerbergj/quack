package ledger

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// unscopedSession is the fallback ledger file for a log record that carries
// no gen_ai.conversation.id (shouldn't happen for a quack-authored event,
// but an exporter must never drop a record it can't file - see it rather
// than silently lose it).
const unscopedSession = "unscoped"

// line is one JSONL entry's on-disk shape: the SDK Record's fields, restated
// plainly - not the OTel wire format, which is oriented at OTLP transport,
// not at a human/tool reading a file back.
type line struct {
	Timestamp time.Time      `json:"timestamp"`
	Severity  string         `json:"severity,omitempty"`
	Body      any            `json:"body,omitempty"`
	Attrs     map[string]any `json:"attributes"`
	TraceID   string         `json:"trace_id,omitempty"`
	SpanID    string         `json:"span_id,omitempty"`
}

// Exporter adapts a LedgerStore to sdklog.Exporter: quack's built-in
// "collector", appending every emitted log record as one redacted JSON line
// to the store, keyed by gen_ai.conversation.id. Recording is best-effort by
// design - Export never returns an error (a store failure is logged at Warn
// and the record dropped), so a broken store can never affect the run.
type Exporter struct {
	store LedgerStore
	log   *slog.Logger
}

// NewExporter wraps store. A nil store is valid - Export then no-ops
// (recording disabled, per config.RecordingConfig.IsEnabled).
func NewExporter(store LedgerStore) *Exporter {
	return &Exporter{store: store, log: slog.With("component", "ledger")}
}

var _ sdklog.Exporter = (*Exporter)(nil)

func (e *Exporter) Export(ctx context.Context, records []sdklog.Record) error {
	if e == nil || e.store == nil {
		return nil
	}
	for _, r := range records {
		body, sessionID, err := encode(r)
		if err != nil {
			e.log.Warn("could not encode a log record; dropping it", "err", err)
			continue
		}
		if err := e.store.Append(ctx, sessionID, body); err != nil {
			// Forbidden: any behavior change to the run when the store errors.
			// This exporter runs off the hot path (a SimpleProcessor call after
			// the model/tool call already completed), so all we owe the run is
			// this warning.
			e.log.Warn("append failed; this event was not recorded", "session", sessionID, "err", err)
		}
	}
	return nil
}

func (e *Exporter) Shutdown(context.Context) error   { return nil }
func (e *Exporter) ForceFlush(context.Context) error { return nil }

// encode converts one SDK log record to a redacted JSON line plus the
// session (chat) id it belongs to.
func encode(r sdklog.Record) (body []byte, sessionID string, err error) {
	attrs := make(map[string]any, r.AttributesLen())
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[string(kv.Key)] = valueToAny(kv.Value)
		return true
	})
	if redacted, ok := Redact(attrs).(map[string]any); ok {
		attrs = redacted
	}
	sessionID, _ = attrs["gen_ai.conversation.id"].(string)
	if sessionID == "" {
		sessionID = unscopedSession
	}

	l := line{
		Timestamp: r.Timestamp(),
		Severity:  r.SeverityText(),
		Body:      valueToAny(r.Body()),
		Attrs:     attrs,
	}
	if tid := r.TraceID(); tid.IsValid() {
		l.TraceID = tid.String()
	}
	if sid := r.SpanID(); sid.IsValid() {
		l.SpanID = sid.String()
	}
	body, err = json.Marshal(l)
	return body, sessionID, err
}

// valueToAny converts an otellog.Value to the generic shape encoding/json
// already knows how to marshal.
func valueToAny(v otellog.Value) any {
	switch v.Kind() {
	case otellog.KindBool:
		return v.AsBool()
	case otellog.KindFloat64:
		return v.AsFloat64()
	case otellog.KindInt64:
		return v.AsInt64()
	case otellog.KindString:
		return v.AsString()
	case otellog.KindBytes:
		return v.AsBytes()
	case otellog.KindSlice:
		s := v.AsSlice()
		out := make([]any, len(s))
		for i, e := range s {
			out[i] = valueToAny(e)
		}
		return out
	case otellog.KindMap:
		m := v.AsMap()
		out := make(map[string]any, len(m))
		for _, kv := range m {
			out[string(kv.Key)] = valueToAny(kv.Value)
		}
		return out
	default:
		return nil
	}
}

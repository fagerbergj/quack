package ledger

import (
	"context"
	"encoding/json"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// Exporter adapts a LedgerStore to sdklog.Exporter: every gen_ai.* log
// record becomes one typed observation Entry. Recording is best-effort by
// design - Export never returns an error (a store failure is logged at Warn
// and the record dropped), so a broken store can never affect the run.
type Exporter struct {
	store LedgerStore
	log   *slog.Logger
}

// NewExporter wraps store. A nil store is valid - Export then no-ops.
func NewExporter(store LedgerStore) *Exporter {
	return &Exporter{store: store, log: slog.With("component", "ledger")}
}

var _ sdklog.Exporter = (*Exporter)(nil)

func (e *Exporter) Export(ctx context.Context, records []sdklog.Record) error {
	if e == nil || e.store == nil {
		return nil
	}
	for _, r := range records {
		entry, ok := EntryFromRecord(r)
		if !ok {
			continue
		}
		if _, err := e.store.AppendIntent(ctx, entry); err != nil {
			// Off the hot path (SimpleProcessor after the call completed); all we owe the run is this warning.
			e.log.Warn("append failed; this event was not recorded", "chat", entry.ChatID, "kind", entry.Kind, "err", err)
		}
	}
	return nil
}

func (e *Exporter) Shutdown(context.Context) error   { return nil }
func (e *Exporter) ForceFlush(context.Context) error { return nil }

// EntryFromRecord converts one gen_ai.* log record into a typed observation
// Entry. ok=false for records no observation kind describes (plan events,
// records without a conversation id) - those still reach any OTLP exporter.
func EntryFromRecord(r sdklog.Record) (Entry, bool) {
	attrs := make(map[string]any, r.AttributesLen())
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = valueToAny(kv.Value)
		return true
	})
	if redacted, ok := Redact(attrs).(map[string]any); ok {
		attrs = redacted
	}
	str := func(k string) string { s, _ := attrs[k].(string); return s }
	num := func(k string) float64 {
		switch v := attrs[k].(type) {
		case int64:
			return float64(v)
		case float64:
			return v
		}
		return 0
	}
	entry := Entry{
		ChatID: str("gen_ai.conversation.id"),
		NodeID: str("quack.node"),
		Agent:  str("gen_ai.agent.name"),
		Round:  str("quack.round"),
		At:     r.Timestamp(),
	}
	if entry.ChatID == "" {
		return Entry{}, false
	}
	var payload any
	switch op := str("gen_ai.operation.name"); {
	case str("gen_ai.evaluation.name") != "":
		entry.Kind = KindEvalScore
		payload = EvalScorePayload{ResponseID: str("gen_ai.response.id"), Criterion: str("gen_ai.evaluation.name"),
			Score: num("gen_ai.evaluation.score.value"), Explanation: str("gen_ai.evaluation.explanation")}
	case op == "chat":
		entry.Kind = KindLLMCall
		finish, _ := attrs["gen_ai.response.finish_reasons"].([]any)
		p := LLMCallPayload{Provider: str("gen_ai.provider.name"), RequestModel: str("gen_ai.request.model"),
			ResponseModel: str("gen_ai.response.model"), ResponseID: str("gen_ai.response.id"),
			InputTokens: int64(num("gen_ai.usage.input_tokens")), OutputTokens: int64(num("gen_ai.usage.output_tokens")),
			Temperature: num("gen_ai.request.temperature"), MaxTokens: int64(num("gen_ai.request.max_tokens")),
			PromptName: str("gen_ai.prompt.name"), PromptVersion: str("gen_ai.prompt.version"),
			SystemInstructions: str("gen_ai.system_instructions"), ToolDefinitions: str("gen_ai.tool.definitions"),
			Input: str("gen_ai.input.messages"), Output: str("gen_ai.output.messages"), Error: str("error.type")}
		if len(finish) > 0 {
			p.FinishReason, _ = finish[0].(string)
		}
		payload = p
	case op == "execute_tool":
		entry.Kind = KindToolCall
		payload = ToolCallPayload{Name: str("gen_ai.tool.name"), Type: str("gen_ai.tool.type"),
			Args: str("gen_ai.tool.call.arguments"), Result: str("gen_ai.tool.call.result"), Error: str("error.type")}
	case op == "invoke_agent":
		entry.Kind = KindAgentInvoke
		payload = AgentInvokePayload{Sent: str("gen_ai.input.messages"), Received: str("gen_ai.output.messages"), Error: str("error.type")}
	default:
		return Entry{}, false
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return Entry{}, false
	}
	entry.Payload = b
	return entry, true
}

// valueToAny converts an attribute.Value to the generic shape encoding/json
// already knows how to marshal. otel/log v0.21.0 dropped its own Value/Kind
// types in favor of attribute.Value/Type (upstream record.go now embeds
// attribute.Value directly) - see the otel/log v0.21.0 release notes.
func valueToAny(v attribute.Value) any {
	switch v.Type() {
	case attribute.BOOL:
		return v.AsBool()
	case attribute.FLOAT64:
		return v.AsFloat64()
	case attribute.INT64:
		return v.AsInt64()
	case attribute.STRING:
		return v.AsString()
	case attribute.BYTESLICE:
		return v.AsByteSlice()
	case attribute.SLICE:
		s := v.AsSlice()
		out := make([]any, len(s))
		for i, e := range s {
			out[i] = valueToAny(e)
		}
		return out
	case attribute.MAP:
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

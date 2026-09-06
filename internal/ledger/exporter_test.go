package ledger

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

func emitVia(t *testing.T, store LedgerStore, attrs ...attribute.KeyValue) {
	t.Helper()
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(NewExporter(store))))
	var rec otellog.Record
	rec.AddAttributes(attrs...)
	provider.Logger("test").Emit(context.Background(), rec)
}

// TestExporterEmitsTypedEntries: a gen_ai chat record becomes one llm.call
// Entry carrying the stream coordinates and a redacted, typed payload.
func TestExporterEmitsTypedEntries(t *testing.T) {
	store := NewMemStore()
	emitVia(t, store,
		attribute.String("gen_ai.conversation.id", "chat-42"),
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", "m1"),
		attribute.String("quack.node", "n1"),
		attribute.String("gen_ai.agent.name", "coder"),
		attribute.String("quack.round", "2"),
		attribute.Slice("gen_ai.response.finish_reasons", attribute.StringValue("stop")),
		attribute.Int64("gen_ai.usage.input_tokens", 7),
		attribute.String("gen_ai.input.messages", `[{"authorization":"Bearer secret"}]`),
	)
	emitVia(t, store,
		attribute.String("gen_ai.conversation.id", "chat-42"),
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", "read_file"),
		attribute.String("gen_ai.tool.call.result", `{"ok":true}`),
	)
	emitVia(t, store,
		attribute.String("gen_ai.conversation.id", "chat-42"),
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.name", "acp"),
	)
	emitVia(t, store,
		attribute.String("gen_ai.conversation.id", "chat-42"),
		attribute.String("gen_ai.evaluation.name", "accuracy"),
		attribute.Float64("gen_ai.evaluation.score.value", 0.8),
	)

	entries, err := store.ReadEntries(context.Background(), "chat-42", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}
	for i, want := range []string{KindLLMCall, KindToolCall, KindAgentInvoke, KindEvalScore} {
		if entries[i].Kind != want {
			t.Errorf("entry %d kind = %q, want %q", i, entries[i].Kind, want)
		}
	}
	e := entries[0]
	if e.NodeID != "n1" || e.Agent != "coder" || e.Round != "2" || e.Seq != 1 {
		t.Errorf("coords = node %q agent %q round %q seq %d", e.NodeID, e.Agent, e.Round, e.Seq)
	}
	var p LLMCallPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.RequestModel != "m1" || p.FinishReason != "stop" || p.InputTokens != 7 {
		t.Errorf("payload = %+v", p)
	}
	if p.Input != `[{"authorization":"[REDACTED]"}]` {
		t.Errorf("input not redacted: %s", p.Input)
	}
	var ev EvalScorePayload
	if err := json.Unmarshal(entries[3].Payload, &ev); err != nil || ev.Criterion != "accuracy" || ev.Score != 0.8 {
		t.Errorf("eval payload = %+v (%v)", ev, err)
	}
}

// TestExporterDropsUnmappedRecords: no conversation id, or an operation no
// observation kind describes, never reaches the store.
func TestExporterDropsUnmappedRecords(t *testing.T) {
	store := NewMemStore()
	emitVia(t, store, attribute.String("gen_ai.operation.name", "chat"))
	emitVia(t, store, attribute.String("gen_ai.conversation.id", "c"), attribute.String("gen_ai.operation.name", "plan"))
	if refs, _ := store.List(context.Background()); len(refs) != 0 {
		t.Fatalf("got %+v, want nothing recorded", refs)
	}
}

func TestExporterDisabledStoreIsNoop(t *testing.T) {
	if err := NewExporter(nil).Export(context.Background(), nil); err != nil {
		t.Fatalf("Export with nil store returned an error: %v", err)
	}
}

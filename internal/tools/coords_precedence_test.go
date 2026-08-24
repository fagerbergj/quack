package tools

import (
	"context"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// #1048: emitTool.Run read only the shared stamp (e.coords), overwriting
// whatever the caller's own ctx carried, unlike traced.go's field-by-field
// merge (#1047) - a concurrent sibling node's stamp could steal this call's
// attribution.
func TestEmitTool_CtxCoordsWinOverTheSharedStamp(t *testing.T) {
	capExp := &recordCapture{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	inner := &fakeRunnable{}
	wrapped, err := emitWrap(inner, ledger.Coords{})
	if err != nil {
		t.Fatalf("emitWrap: %v", err)
	}
	e := wrapped.(*emitTool)

	// A concurrent sibling node stamped last and is still "current" on this
	// shared tool instance.
	e.SetLedgerCoords(ledger.Coords{ChatID: "chat-1", Node: "sibling-node", Agent: "judge"})

	fc := newFakeCtx()
	fc.Ctx = ledger.WithCoords(context.Background(), ledger.Coords{Node: "my-node", Agent: "code-implementer"})

	if _, err := e.Run(fc, map[string]any{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := map[string]otellog.Value{}
	capExp.records[0].WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if got := attrs[otelobs.GenAIAgentName].AsString(); got != "code-implementer" {
		t.Errorf("gen_ai.agent.name = %q, want code-implementer (ctx must win over the sibling's stamp)", got)
	}
}

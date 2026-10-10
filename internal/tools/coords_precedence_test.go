package tools

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// The caller's ctx coords must win over the hooks' shared stamp, or a concurrent sibling node's stamp
// steals this call's attribution.
func TestEmitTool_CtxCoordsWinOverTheSharedStamp(t *testing.T) {
	capExp := &recordCapture{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	h := NewHooks(Deps{}, 0)
	e := hook(h, HookBuilt, &fakeRunnable{})

	// A concurrent sibling node stamped last on this shared hooks instance.
	h.SetLedgerCoords(ledger.Coords{ChatID: "chat-1", Node: "sibling-node", Agent: "judge"})

	fc := newFakeCtx()
	fc.Ctx = ledger.WithCoords(context.Background(), ledger.Coords{Node: "my-node", Agent: "code-implementer"})

	if _, err := e.Run(fc, map[string]any{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(capExp.records) != 1 {
		t.Fatalf("got %d records, want 1", len(capExp.records))
	}
	attrs := map[string]attribute.Value{}
	capExp.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	if got := attrs[otelobs.GenAIAgentName].AsString(); got != "code-implementer" {
		t.Errorf("gen_ai.agent.name = %q, want code-implementer (ctx must win over the sibling's stamp)", got)
	}
}

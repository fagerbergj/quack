package inference

import (
	"context"
	"iter"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// One tracedModel is shared by every node using that model (always the judge), so a caller's own
// ctx coords must beat whichever node stamped last, or usage lands on the wrong node.
func TestTracedModel_CtxCoordsWinOverTheSharedStamp(t *testing.T) {
	p := &probeLLM{}
	m := TracedModelForTesting(p, "shared-judge-model")
	stamper := m.(interface{ SetLedgerCoords(ledger.Coords) })

	// A concurrent sibling node stamped last and is still running.
	stamper.SetLedgerCoords(ledger.Coords{Node: "sibling-node", Agent: "judge"})

	// This call carries its own coords in ctx, the way runJudgeAgent does.
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Node: "my-node", Agent: "judge"})
	for range m.GenerateContent(ctx, &model.LLMRequest{}, false) {
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) != 1 {
		t.Fatalf("want 1 call, got %d", len(p.seen))
	}
	if p.seen[0].Node != "my-node" {
		t.Errorf("call attributed to %q; the caller's own ctx coords must win over another node's stamp", p.seen[0].Node)
	}
}

// The stamp is still the fallback: the worker path needs it because RunNode
// rebuilds the child context and drops ctx coords.
func TestTracedModel_StampStillAppliesWhenCtxHasNoCoords(t *testing.T) {
	p := &probeLLM{}
	m := TracedModelForTesting(p, "worker-model")
	m.(interface{ SetLedgerCoords(ledger.Coords) }).SetLedgerCoords(ledger.Coords{Node: "worker-node"})

	for range m.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) != 1 || p.seen[0].Node != "worker-node" {
		t.Fatalf("stamp must still fill in when ctx carries no coords; got %v", p.seen)
	}
}

// Production never starts from a bare ctx: the run carries ChatID/User/Source with no node, which an
// all-or-nothing check would treat as authoritative, dropping node attribution on every worker call.
func TestTracedModel_OuterRunCoordsDoNotSuppressTheStamp(t *testing.T) {
	p := &probeLLM{}
	m := TracedModelForTesting(p, "worker-model")
	m.(interface{ SetLedgerCoords(ledger.Coords) }).SetLedgerCoords(ledger.Coords{
		ChatID: "chat-1", Node: "n1", Agent: "web-researcher", Round: "worker-r0", User: "u", Source: "ui",
	})

	// What RunPlanAsGraph actually hands down: the run's partial coords.
	outer := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-1", User: "u", Source: "ui"})
	for range m.GenerateContent(outer, &model.LLMRequest{}, false) {
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	got := p.seen[0]
	if got.Node != "n1" || got.Agent != "web-researcher" || got.Round != "worker-r0" {
		t.Fatalf("worker call lost its node attribution: %+v", got)
	}
	if got.ChatID != "chat-1" || got.Source != "ui" {
		t.Errorf("outer run fields must survive: %+v", got)
	}
}

// A field the caller set is never overwritten by another node's stamp, even
// when the rest of the stamp is filling blanks.
func TestTracedModel_StampNeverOverwritesAFieldTheCallerSet(t *testing.T) {
	p := &probeLLM{}
	m := TracedModelForTesting(p, "shared-judge-model")
	m.(interface{ SetLedgerCoords(ledger.Coords) }).SetLedgerCoords(ledger.Coords{
		ChatID: "chat-1", Node: "sibling-node", Agent: "judge", Round: "judge-r1",
	})

	ctx := ledger.WithCoords(context.Background(), ledger.Coords{Node: "my-node", Agent: "judge"})
	for range m.GenerateContent(ctx, &model.LLMRequest{}, false) {
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	got := p.seen[0]
	if got.Node != "my-node" {
		t.Errorf("node = %q; a sibling's stamp must not steal it", got.Node)
	}
	if got.ChatID != "chat-1" || got.Round != "judge-r1" {
		t.Errorf("blanks should still be filled from the stamp: %+v", got)
	}
}

// probeLLM records the coords in ctx when each call ran.
type probeLLM struct {
	mu   sync.Mutex
	seen []ledger.Coords
}

func (p *probeLLM) Name() string { return "probe" }

func (p *probeLLM) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	p.mu.Lock()
	p.seen = append(p.seen, ledger.CoordsFromContext(ctx))
	p.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {}
}

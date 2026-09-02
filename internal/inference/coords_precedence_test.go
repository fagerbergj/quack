package inference

import (
	"context"
	"iter"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// probeLLM records the node coords that were actually in ctx when the call ran.
type probeLLM struct {
	mu   sync.Mutex
	seen []string
}

func (p *probeLLM) Name() string { return "probe" }

func (p *probeLLM) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	p.mu.Lock()
	p.seen = append(p.seen, ledger.CoordsFromContext(ctx).Node)
	p.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {}
}

// #1039: one tracedModel instance is shared by every node using that model -
// the judge model by definition, since every gated node judges with it. The
// shared stamp used to OVERWRITE per-call ctx coords, so a caller that had
// already put the right node in ctx (vetting's judge round does exactly that,
// via ledger.WithCoords) still got whatever node stamped last. Ledger events,
// tokens and cost then land on the wrong node. Mutex-guarded, so -race never
// sees it.
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
	if p.seen[0] != "my-node" {
		t.Errorf("call attributed to %q; the caller's own ctx coords must win over another node's stamp", p.seen[0])
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
	if len(p.seen) != 1 || p.seen[0] != "worker-node" {
		t.Fatalf("stamp must still fill in when ctx carries no coords; got %v", p.seen)
	}
}

// The case that let the first attempt through: production NEVER starts from a
// bare context. The orchestrator stamps partial coords on the run ctx
// (orchestrator.go:404 - ChatID/User/Source, no node), and those survive down to
// the model call. An all-or-nothing "does ctx have coords?" check treats that as
// authoritative and drops node/agent/round on every worker call - attributing to
// NO node, which is worse than attributing to the wrong one.
func TestTracedModel_OuterRunCoordsDoNotSuppressTheStamp(t *testing.T) {
	p := &probeLLMFull{}
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
	p := &probeLLMFull{}
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

type probeLLMFull struct {
	mu   sync.Mutex
	seen []ledger.Coords
}

func (p *probeLLMFull) Name() string { return "probe-full" }

func (p *probeLLMFull) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	p.mu.Lock()
	p.seen = append(p.seen, ledger.CoordsFromContext(ctx))
	p.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {}
}

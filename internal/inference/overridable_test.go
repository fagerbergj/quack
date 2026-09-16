package inference

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

type stubLLM struct{ name string }

func (s stubLLM) Name() string { return s.name }
func (s stubLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {}
}

type coordsStubLLM struct {
	stubLLM
	last ledger.Coords
}

func (s *coordsStubLLM) SetLedgerCoords(c ledger.Coords) { s.last = c }

func TestOverridableModelSet(t *testing.T) {
	m := NewOverridable(stubLLM{name: "base"})
	if got := m.Name(); got != "base" {
		t.Fatalf("Name() = %q, want base", got)
	}
	m.Set(stubLLM{name: "bound"})
	if got := m.Name(); got != "bound" {
		t.Fatalf("Name() = %q, want bound after Set", got)
	}
}

func TestOverridableModelSetLedgerCoords(t *testing.T) {
	// No target implements SetLedgerCoords: must not panic.
	m := NewOverridable(stubLLM{name: "base"})
	m.SetLedgerCoords(ledger.Coords{ChatID: "c1"})

	target := &coordsStubLLM{stubLLM: stubLLM{name: "bound"}}
	m.Set(target)
	m.SetLedgerCoords(ledger.Coords{ChatID: "c2"})
	if target.last.ChatID != "c2" {
		t.Fatalf("target.last = %+v, want ChatID c2", target.last)
	}
}

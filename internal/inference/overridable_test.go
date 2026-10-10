package inference

import (
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
)

type coordsStubLLM struct {
	stubModel
	last ledger.Coords
}

func (s *coordsStubLLM) SetLedgerCoords(c ledger.Coords) { s.last = c }

func TestOverridableModelSet(t *testing.T) {
	m := NewOverridable(&stubModel{name: "base"})
	if got := m.Name(); got != "base" {
		t.Fatalf("Name() = %q, want base", got)
	}
	m.Set(&stubModel{name: "bound"})
	if got := m.Name(); got != "bound" {
		t.Fatalf("Name() = %q, want bound after Set", got)
	}
}

func TestOverridableModelSetLedgerCoords(t *testing.T) {
	// No target implements SetLedgerCoords: must not panic.
	m := NewOverridable(&stubModel{name: "base"})
	m.SetLedgerCoords(ledger.Coords{ChatID: "c1"})

	target := &coordsStubLLM{stubModel: stubModel{name: "bound"}}
	m.Set(target)
	m.SetLedgerCoords(ledger.Coords{ChatID: "c2"})
	if target.last.ChatID != "c2" {
		t.Fatalf("target.last = %+v, want ChatID c2", target.last)
	}
}

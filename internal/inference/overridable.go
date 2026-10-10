package inference

import (
	"context"
	"iter"
	"sync/atomic"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// OverridableModel lets a worker's model binding change at a round boundary without
// rebuilding the ADK agent holding it.
type OverridableModel struct {
	target atomic.Pointer[model.LLM]
}

func NewOverridable(base model.LLM) *OverridableModel {
	m := &OverridableModel{}
	m.Set(base)
	return m
}

func (m *OverridableModel) Set(target model.LLM) { m.target.Store(&target) }

func (m *OverridableModel) Get() model.LLM { return m.current() }

func (m *OverridableModel) current() model.LLM { return *m.target.Load() }

func (m *OverridableModel) Name() string { return m.current().Name() }

// GenerateContent: Set only happens between rounds, so a round sees one target throughout.
func (m *OverridableModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return m.current().GenerateContent(ctx, req, stream)
}

func (m *OverridableModel) SetLedgerCoords(c ledger.Coords) {
	if cs, ok := m.current().(interface{ SetLedgerCoords(ledger.Coords) }); ok {
		cs.SetLedgerCoords(c)
	}
}

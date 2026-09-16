package inference

import (
	"context"
	"iter"
	"sync/atomic"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// OverridableModel delegates to whichever model.LLM Set last installed, so a
// worker's model/provider/effort binding (#1421 P2) can change at a round
// boundary without rebuilding the ADK agent that holds this LLM.
type OverridableModel struct {
	target atomic.Pointer[model.LLM]
}

// NewOverridable wraps base as the initial target.
func NewOverridable(base model.LLM) *OverridableModel {
	m := &OverridableModel{}
	m.Set(base)
	return m
}

// Set installs target as what every subsequent call delegates to.
func (m *OverridableModel) Set(target model.LLM) { m.target.Store(&target) }

// Get returns the current delegate, e.g. to revert to it after a temporary override.
func (m *OverridableModel) Get() model.LLM { return m.current() }

func (m *OverridableModel) current() model.LLM { return *m.target.Load() }

// Name reports the CURRENT target's name, e.g. a bound-in override.
func (m *OverridableModel) Name() string { return m.current().Name() }

// GenerateContent runs on the current target - a round always sees whatever
// was pinned at its start, since Set only ever happens between rounds.
func (m *OverridableModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return m.current().GenerateContent(ctx, req, stream)
}

// SetLedgerCoords forwards to the current target when it supports it -
// mirrors the tracedModel stamp every other model.LLM in this package carries.
func (m *OverridableModel) SetLedgerCoords(c ledger.Coords) {
	if cs, ok := m.current().(interface{ SetLedgerCoords(ledger.Coords) }); ok {
		cs.SetLedgerCoords(c)
	}
}

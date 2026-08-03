package ledger

import "context"

// Coords are the execution coordinates a ledger event is stamped with:
// which chat/session, which DAG node, which agent, and which round within
// that node (a runID like "worker-r0" or "judge-r1" - the same identifier
// dagStream already groups by, so replay's stream grouping needs nothing new).
type Coords struct {
	ChatID string
	Node   string
	Agent  string
	Round  string
}

type coordsKey struct{}

// WithCoords stamps c onto ctx. The vetting gate (the one place that knows
// node/agent/round) sets this once per worker/judge round; every seam below
// it (a model call, a tool call) reads it back via CoordsFromContext without
// its own signature needing to carry the identity.
func WithCoords(ctx context.Context, c Coords) context.Context {
	return context.WithValue(ctx, coordsKey{}, c)
}

// CoordsFromContext returns the coordinates stamped on ctx, or the zero
// value if none were set (a call outside any gated node - e.g. a direct
// tool invocation).
func CoordsFromContext(ctx context.Context) Coords {
	if ctx == nil {
		return Coords{}
	}
	c, _ := ctx.Value(coordsKey{}).(Coords)
	return c
}

// CoordSetter is implemented by an emission wrapper that can be
// re-stamped with fresh coordinates after construction - the model
// (inference.tracedModel) and tool (tools.emitTool) seams both do, because
// workflow.RunNode's dynamic-child scheduling does not propagate a
// context.WithValue stamp down to the call underneath it (see
// tracedModel.SetLedgerCoords's doc comment for the full explanation).
type CoordSetter interface {
	SetLedgerCoords(Coords)
}

// StampCoords applies c to every element of items that implements
// CoordSetter, silently skipping the rest. Generic over T so a caller can
// pass e.g. []tool.Tool without this package importing ADK's tool
// package - ledger has no reason to know what a "tool" is.
func StampCoords[T any](items []T, c Coords) {
	for _, it := range items {
		if cs, ok := any(it).(CoordSetter); ok {
			cs.SetLedgerCoords(c)
		}
	}
}

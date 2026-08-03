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

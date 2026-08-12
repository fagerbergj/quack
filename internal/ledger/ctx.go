package ledger

import "context"

type Coords struct {
	ChatID string
	Node   string
	Agent  string
	Round  string
	// User: the ADK session identity that owns this run (local user, GitHub
	// commenter login, etc) - observability attribution only.
	User string
	// Source: the run's origin - an extension's registration name for an
	// extension-dispatched run, or a fixed value for direct UI/REST/MCP
	// chats. Bounded cardinality by construction; never chat_id or node_id.
	Source string
}

type coordsKey struct{}

// WithCoords stamps execution coordinates onto ctx; seams below read via CoordsFromContext.
func WithCoords(ctx context.Context, c Coords) context.Context {
	return context.WithValue(ctx, coordsKey{}, c)
}

// CoordsFromContext returns coordinates or zero value.
func CoordsFromContext(ctx context.Context) Coords {
	if ctx == nil {
		return Coords{}
	}
	c, _ := ctx.Value(coordsKey{}).(Coords)
	return c
}

// CoordSetter lets emission wrappers be re-stamped with fresh coordinates after construction.
type CoordSetter interface {
	SetLedgerCoords(Coords)
}

// StampCoords applies c to every CoordSetter in items; generic over T to avoid ADK imports.
func StampCoords[T any](items []T, c Coords) {
	for _, it := range items {
		if cs, ok := any(it).(CoordSetter); ok {
			cs.SetLedgerCoords(c)
		}
	}
}

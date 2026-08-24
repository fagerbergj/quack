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

// FillBlankCoords: ctx wins per field, stamp fills what ctx left empty. A
// stamp shared by every node on one model/tool/agent must never overwrite a
// field the caller's own ctx already set (#1039, #1048).
func FillBlankCoords(ctx, stamp Coords) Coords {
	if ctx.ChatID == "" {
		ctx.ChatID = stamp.ChatID
	}
	if ctx.Node == "" {
		ctx.Node = stamp.Node
	}
	if ctx.Agent == "" {
		ctx.Agent = stamp.Agent
	}
	if ctx.Round == "" {
		ctx.Round = stamp.Round
	}
	if ctx.User == "" {
		ctx.User = stamp.User
	}
	if ctx.Source == "" {
		ctx.Source = stamp.Source
	}
	return ctx
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

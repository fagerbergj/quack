package ledger

import (
	"context"

	oteltrace "go.opentelemetry.io/otel/trace"
)

type Coords struct {
	ChatID string
	Node   string
	Agent  string
	Round  string
	// BundleHash: the acting agent's bundle content hash (agent.Bundle.Hash),
	// stamped alongside Agent - provenance for which prompt version ran.
	BundleHash string
	// PromptSource/PromptVersionID: the store the round's system prompt came from ("static" or a prompts:
	// store) and its version there.
	PromptSource    string
	PromptVersionID string
	// PromptArtifact is derived from the bundle directory, not the agent name, so a renamed or out-of-tree
	// bundle still records the right name.
	PromptArtifact string
	// Artifacts/Plugins: every artifact and plugin this round resolved through
	// artifactsrc/the plugin registry - the llm.call/agent.invoke provenance list.
	Artifacts []ArtifactRef
	Plugins   []PluginRef
	// User: the ADK session identity that owns this run (local user, GitHub
	// commenter login, etc) - observability attribution only.
	User string
	// Source is the run's origin (extension name or a fixed value for direct chats). Bounded cardinality:
	// never chat_id or node_id.
	Source string
	// SpanContext is captured before ADK rebuilds the ctx (SpanFromContext no-ops after). Zero means no
	// linkage; consumers must degrade gracefully.
	SpanContext oteltrace.SpanContext
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

// FillBlankCoords: ctx wins per field and stamp fills only what ctx left empty, so a stamp shared by every
// node never overwrites the caller's own coords.
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
	if ctx.BundleHash == "" {
		ctx.BundleHash = stamp.BundleHash
	}
	if ctx.PromptSource == "" {
		ctx.PromptSource = stamp.PromptSource
	}
	if ctx.PromptVersionID == "" {
		ctx.PromptVersionID = stamp.PromptVersionID
	}
	if ctx.PromptArtifact == "" {
		ctx.PromptArtifact = stamp.PromptArtifact
	}
	if len(ctx.Artifacts) == 0 {
		ctx.Artifacts = stamp.Artifacts
	}
	if len(ctx.Plugins) == 0 {
		ctx.Plugins = stamp.Plugins
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
	if !ctx.SpanContext.IsValid() {
		// stamp.SpanContext must be the round's OWN span - a wrong one silently mis-parents traces.
		ctx.SpanContext = stamp.SpanContext
	}
	return ctx
}

// IsZero reports whether c is the unset value. SpanContext isn't Go-comparable
// (its TraceState wraps a slice), so this can't be a plain `== Coords{}`.
func (c Coords) IsZero() bool {
	return c.ChatID == "" && c.Node == "" && c.Agent == "" && c.Round == "" &&
		c.BundleHash == "" && c.PromptSource == "" && c.PromptVersionID == "" && c.PromptArtifact == "" &&
		c.User == "" && c.Source == "" && !c.SpanContext.IsValid()
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

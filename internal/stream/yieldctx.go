package stream

import "context"

type yieldCtxKey struct{}

// WithYield stores fn in ctx so tools called from the orchestrator can
// forward SSE events up through the outer SSE stream without going through
// the ADK session event pipeline. fn MUST already serialize its callers: concurrent DAG nodes call it from their own goroutines (dag/graph.go's onQueued, vetting's stage spans), and an unsynchronized fn panics the process (#1016, #1032).
func WithYield(ctx context.Context, fn func(SSEEvent)) context.Context {
	return context.WithValue(ctx, yieldCtxKey{}, fn)
}

// YieldFromContext retrieves the yield function stored by WithYield, or returns
// false if none was stored.
func YieldFromContext(ctx context.Context) (func(SSEEvent), bool) {
	fn, ok := ctx.Value(yieldCtxKey{}).(func(SSEEvent))
	return fn, ok
}

type turnIDCtxKey struct{}

// WithTurnID stores the chat turn id (the response_created response_id) that a
// run belongs to, so artifacts saved during it can be placed on that turn.
func WithTurnID(ctx context.Context, turnID string) context.Context {
	return context.WithValue(ctx, turnIDCtxKey{}, turnID)
}

// TurnIDFromContext returns the id WithTurnID stored, or "".
func TurnIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(turnIDCtxKey{}).(string)
	return id
}

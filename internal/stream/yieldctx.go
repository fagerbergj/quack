package stream

import "context"

type yieldCtxKey struct{}

// WithYield lets orchestrator tools forward SSE events outside the ADK session pipeline. fn MUST serialize
// its callers: concurrent DAG nodes call it from their own goroutines, and an unsynchronized fn panics.
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

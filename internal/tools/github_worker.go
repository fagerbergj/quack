package tools

import (
	"context"

	"github.com/fagerbergj/quack/internal/dag"
)

// Trigger-computed values threaded from the dispatch boundary to the plan tools; the model never authors them.

type workerAskContextKey struct{}

// WithWorkerAsk: the ask-only text (never evidence) a triggered plan's nodes get as background instead of
// the orchestrator's envelope. Call only from the webhook dispatch boundary.
func WithWorkerAsk(ctx context.Context, ask string) context.Context {
	return context.WithValue(ctx, workerAskContextKey{}, ask)
}

// WorkerAskFromContext: "" means no trigger, so buildTask falls back to UserMessage.
func WorkerAskFromContext(ctx context.Context) string {
	s, _ := ctx.Value(workerAskContextKey{}).(string)
	return s
}

type contextItemsContextKey struct{}

// WithContextItems: a CI-fix run's failing checks, computed at dispatch; buildTask hands an item's detail
// only to the node whose task names it.
func WithContextItems(ctx context.Context, items []dag.ContextItem) context.Context {
	return context.WithValue(ctx, contextItemsContextKey{}, items)
}

func ContextItemsFromContext(ctx context.Context) []dag.ContextItem {
	c, _ := ctx.Value(contextItemsContextKey{}).([]dag.ContextItem)
	return c
}

type planOnlyContextKey struct{}

// WithPlanOnly: the quack:plan label, never a model's claim; buildGateNodes then forces every node
// read-only with no delivery target, whatever agent the planner picks.
func WithPlanOnly(ctx context.Context, planOnly bool) context.Context {
	return context.WithValue(ctx, planOnlyContextKey{}, planOnly)
}

func PlanOnlyFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(planOnlyContextKey{}).(bool)
	return v
}

type nodeStoppedContextKey struct{}

// WithNodeStopped attaches the run's "did the user stop this node before it delivered" check.
func WithNodeStopped(ctx context.Context, stopped func(nodeID string) bool) context.Context {
	return context.WithValue(ctx, nodeStoppedContextKey{}, stopped)
}

// NodeStoppedFromContext reads back WithNodeStopped's check; without one, nothing is stopped.
func NodeStoppedFromContext(ctx context.Context) func(nodeID string) bool {
	if f, ok := ctx.Value(nodeStoppedContextKey{}).(func(string) bool); ok {
		return f
	}
	return func(string) bool { return false }
}

package tools

import (
	"context"
)

type allowedDeliveryKindsContextKey struct{}

// WithAllowedDeliveryKinds attaches the trigger's computed delivery-kind
// allowlist (#657, #662) to ctx. Call ONLY from the GitHub webhook (or an
// extension dispatch), before it dispatches the orchestrator - the allowlist is computed from labels/authorship/fork state and never re-derived from model output.
func WithAllowedDeliveryKinds(ctx context.Context, kinds []string) context.Context {
	return context.WithValue(ctx, allowedDeliveryKindsContextKey{}, kinds)
}

// AllowedDeliveryKindsFromContext reads back the allowlist
// WithAllowedDeliveryKinds attached, if any. Read exactly ONCE at the top of
// Orchestrator.Run and threaded as a plain closed-over value (see GitHubPRFromContext), not trusted to survive deep in the tool-call plumbing; nil = no trigger governs this run (plain REST/MCP) - unrestricted delivery.
func AllowedDeliveryKindsFromContext(ctx context.Context) []string {
	kinds, _ := ctx.Value(allowedDeliveryKindsContextKey{}).([]string)
	return kinds
}

package tools

import (
	"context"
)

type allowedDeliveryKindsContextKey struct{}

// WithAllowedDeliveryKinds attaches the trigger's computed delivery-kind
// allowlist (#657, #662) to ctx. Call ONLY from the GitHub webhook (or an
// extension dispatch), before it dispatches the orchestrator - the allowlist
// is computed from labels/authorship/fork state at that boundary and never
// re-derived from model output.
func WithAllowedDeliveryKinds(ctx context.Context, kinds []string) context.Context {
	return context.WithValue(ctx, allowedDeliveryKindsContextKey{}, kinds)
}

// AllowedDeliveryKindsFromContext reads back the allowlist
// WithAllowedDeliveryKinds attached, if any. Read exactly ONCE, at the top of
// Orchestrator.Run, and threaded in as a plain closed-over value (see
// GitHubPRFromContext's same contract) rather than trusted to survive deep
// inside the agent runtime's tool-call plumbing. nil means no trigger governs
// this run (a plain REST/MCP turn) - unrestricted delivery.
func AllowedDeliveryKindsFromContext(ctx context.Context) []string {
	kinds, _ := ctx.Value(allowedDeliveryKindsContextKey{}).([]string)
	return kinds
}

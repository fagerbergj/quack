package tools

import (
	"context"

	"github.com/fagerbergj/quack/internal/vetting"
)

type grantContextKey struct{}

// WithGrant attaches the trigger's computed permission grant (#657, #662) to
// ctx. Call ONLY from the GitHub webhook, before it dispatches the
// orchestrator - the grant is computed from labels/authorship/fork state at
// that boundary and never re-derived from model output.
func WithGrant(ctx context.Context, g vetting.Grant) context.Context {
	return context.WithValue(ctx, grantContextKey{}, &g)
}

// GrantFromContext reads back the grant WithGrant attached, if any. Read
// exactly ONCE, at the top of Orchestrator.Run, and threaded in as a plain
// closed-over value (see GitHubPRFromContext's same contract) rather than
// trusted to survive deep inside the agent runtime's tool-call plumbing. nil
// means no GitHub trigger governs this run (a plain REST/MCP turn) -
// unrestricted delivery.
func GrantFromContext(ctx context.Context) *vetting.Grant {
	g, _ := ctx.Value(grantContextKey{}).(*vetting.Grant)
	return g
}

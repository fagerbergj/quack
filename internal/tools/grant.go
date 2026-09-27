package tools

import (
	"context"
	"encoding/json"
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

// originGrantKey records an extension chat's latest dispatch grant in its origin
// JSON, so a later REST turn on that chat runs under the same grant.
const originGrantKey = "quackAllowedDeliveryKinds"

// WithOriginGrant returns originJSON with kinds recorded; nil kinds (unrestricted) removes the record.
func WithOriginGrant(originJSON string, kinds []string) string {
	m := map[string]json.RawMessage{}
	if originJSON != "" && json.Unmarshal([]byte(originJSON), &m) != nil {
		return originJSON
	}
	delete(m, originGrantKey)
	if kinds != nil {
		m[originGrantKey], _ = json.Marshal(kinds)
	}
	if len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// OriginGrant reads the grant WithOriginGrant recorded; ok=false means none (unrestricted).
func OriginGrant(originJSON string) (kinds []string, ok bool) {
	var v struct {
		Kinds *[]string `json:"quackAllowedDeliveryKinds"`
	}
	if json.Unmarshal([]byte(originJSON), &v) != nil || v.Kinds == nil {
		return nil, false
	}
	if *v.Kinds == nil {
		return []string{}, true
	}
	return *v.Kinds, true
}

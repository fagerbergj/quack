package tools

import (
	"context"
	"encoding/json"
)

type allowedDeliveryKindsContextKey struct{}

// WithAllowedDeliveryKinds: call only from a webhook or extension dispatch; the allowlist comes from
// labels/authorship/fork state, never from model output.
func WithAllowedDeliveryKinds(ctx context.Context, kinds []string) context.Context {
	return context.WithValue(ctx, allowedDeliveryKindsContextKey{}, kinds)
}

// AllowedDeliveryKindsFromContext is read once at the top of Orchestrator.Run, not trusted deeper in the
// tool-call plumbing; nil means no trigger governs the run (unrestricted).
func AllowedDeliveryKindsFromContext(ctx context.Context) []string {
	kinds, _ := ctx.Value(allowedDeliveryKindsContextKey{}).([]string)
	return kinds
}

// originGrantKey records an extension chat's latest dispatch grant in its origin
// JSON, so a later REST turn on that chat runs under the same grant.
const originGrantKey = "quackAllowedDeliveryKinds"

// WithOriginGrant: nil kinds (a nudge with no Delivery) keeps the grant already recorded.
func WithOriginGrant(originJSON string, kinds []string) string {
	m := map[string]json.RawMessage{}
	if kinds == nil || (originJSON != "" && json.Unmarshal([]byte(originJSON), &m) != nil) {
		return originJSON
	}
	m[originGrantKey], _ = json.Marshal(kinds)
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

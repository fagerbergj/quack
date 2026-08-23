// Package auth enforces Quack's inbound request auth: a chi middleware that
// trusts a forward-auth gateway's headers, or verifies an OIDC bearer token,
// depending on config.AuthConfig. Unconfigured (nil), it is a no-op — every
// request passes through unauthenticated, matching pre-auth behavior.
package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fagerbergj/quack/internal/config"
)

// Identity is the caller identity the middleware attaches to the request
// context, sourced from a trusted header or a verified bearer token's claims.
type Identity struct {
	User   string
	Groups []string
}

type identityCtxKey struct{}

func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// Auth enforces the configured policy. A nil *Auth (auth: absent from config)
// is valid, and Middleware on it is a passthrough.
type Auth struct {
	trustedUserHeader   string
	trustedGroupsHeader string
	verifier            *oidcVerifier // nil when oidc: is not configured
}

// New builds the enforcement from cfg. A nil cfg returns a nil *Auth
// (disabled). When cfg.OIDC is set, this fetches discovery (and, unless
// jwks_url overrides it, the JWKS) synchronously — a bad issuer is a startup
// error, not a silent 401 factory discovered on the first request.
func New(cfg *config.InboundAuthConfig) (*Auth, error) {
	if cfg == nil {
		return nil, nil
	}
	a := &Auth{}
	if cfg.TrustedHeaders != nil {
		a.trustedUserHeader = cfg.TrustedHeaders.User
		a.trustedGroupsHeader = cfg.TrustedHeaders.Groups
	}
	if cfg.OIDC != nil {
		v, err := newOIDCVerifier(cfg.OIDC)
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
		a.verifier = v
	}
	return a, nil
}

// Middleware enforces the configured policy on every request it wraps. A nil
// *Auth is a no-op passthrough. Per request: a trusted header (if configured
// and present) wins outright — the gateway already authenticated it; else a
// configured oidc verifier requires and checks a bearer token; else (auth
// configured but neither path satisfied) the request is unauthorized.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := a.trustedIdentity(r); ok {
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
			return
		}
		if a.verifier != nil {
			id, err := a.verifier.verifyRequest(r)
			if err != nil {
				// Bearer verification failure is an expected client-side condition
				// (expired/malformed/wrong-audience token) - detail goes to the log,
				// never the response, so an unauthenticated caller doesn't learn
				// anything about the verifier's internals.
				slog.Warn("bearer token rejected", "component", "auth", "err", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// trustedIdentity reads the configured trusted headers off r. ok is false
// when trusted_headers isn't configured, or this particular request doesn't
// carry the user header — falling through to bearer-token verification.
func (a *Auth) trustedIdentity(r *http.Request) (Identity, bool) {
	if a.trustedUserHeader == "" {
		return Identity{}, false
	}
	user := r.Header.Get(a.trustedUserHeader)
	if user == "" {
		return Identity{}, false
	}
	id := Identity{User: user}
	if a.trustedGroupsHeader != "" {
		if raw := r.Header.Get(a.trustedGroupsHeader); raw != "" {
			id.Groups = splitGroups(raw)
		}
	}
	return id, true
}

// splitGroups parses a gateway's comma-separated groups header value.
func splitGroups(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// bearerToken extracts the token from "Authorization: Bearer <token>", or ""
// if the header is absent or a different scheme.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix))
}

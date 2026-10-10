// Package auth is quack's inbound auth middleware: trusted forward-auth headers or an OIDC bearer token,
// per config.AuthConfig. Unconfigured (nil), every request passes unauthenticated.
package auth

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fagerbergj/quack/internal/config"
)

// Auth enforces the configured policy. A nil *Auth (auth: absent from config)
// is valid, and Middleware on it is a passthrough.
type Auth struct {
	trustedUserHeader string
	verifier          *oidcVerifier // nil when oidc: is not configured
}

// New builds the enforcement from cfg; nil cfg means disabled. With OIDC it fetches discovery (and JWKS)
// synchronously, so a bad issuer is a startup error rather than a 401 on every request.
func New(cfg *config.InboundAuthConfig) (*Auth, error) {
	if cfg == nil {
		return nil, nil
	}
	a := &Auth{}
	if cfg.TrustedHeaders != nil {
		a.trustedUserHeader = cfg.TrustedHeaders.User
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

// Middleware enforces the policy (a nil *Auth passes through): a present trusted header wins, else a
// configured OIDC verifier requires a valid bearer token, else the request is unauthorized.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.trustedHeaderPresent(r) {
			next.ServeHTTP(w, r)
			return
		}
		if a.verifier != nil {
			if err := a.verifier.verifyRequest(r); err != nil {
				// A rejected bearer token is an expected client condition: detail goes to the log,
				// never the response, so callers learn nothing about the verifier.
				slog.Warn("bearer token rejected", "component", "auth", "err", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// trustedHeaderPresent: false when no trusted header is configured or the request lacks it,
// falling through to bearer verification.
func (a *Auth) trustedHeaderPresent(r *http.Request) bool {
	return a.trustedUserHeader != "" && r.Header.Get(a.trustedUserHeader) != ""
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

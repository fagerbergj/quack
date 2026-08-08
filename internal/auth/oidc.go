package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/zitadel/oidc/v3/pkg/client"
	"github.com/zitadel/oidc/v3/pkg/client/rp"
	"github.com/zitadel/oidc/v3/pkg/oidc"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/httpx"
)

// discoveryTimeout bounds the whole startup probe (discovery + JWKS fetch,
// including the shared transport's own retries) - a hung IdP must not hang
// server boot indefinitely.
const discoveryTimeout = 10 * time.Second

// oidcProbeAttempts + oidcProbeBackoff configure the shared resilient
// transport's retry: a transient IdP blip (a rolling deploy landing
// mid-startup-probe, say) gets a couple of retries instead of hard-failing
// on the first bad request. This only widens the window - once
// oidcProbeAttempts is exhausted, a genuinely unreachable or misconfigured
// issuer still fails startup; it never falls back to running open.
// oidcProbeBackoff is a var so tests don't have to wait out real backoff.
const oidcProbeAttempts = 3

var oidcProbeBackoff = time.Second

// oidcVerifier verifies a bearer token's signature, issuer, audience, and
// expiry via zitadel/oidc's rp.IDTokenVerifier. rp.NewIDTokenVerifier and
// rp.NewRemoteKeySet are self-contained primitives - no RelyingParty, no
// browser/redirect flow, no auth code exchange - built from just
// issuer+audience+JWKS, which is all a resource server verifying bearer
// tokens needs.
type oidcVerifier struct {
	issuer   string
	audience string
	verifier *rp.IDTokenVerifier
}

// newOIDCVerifier resolves the signing-key source - jwks_url if the config
// overrides discovery, else <issuer>/.well-known/openid-configuration's
// jwks_uri (via client.Discover, which also rejects a discovery doc whose own
// "issuer" doesn't match cfg.Issuer) - and probes it. Both are GETs, so the
// shared transport (internal/httpx) retries a transient failure up to
// oidcProbeAttempts times on its own; this fold-in replaces what used to be
// a hand-rolled outer retry loop here. rp.NewRemoteKeySet itself fetches
// lazily on first use with its own background-refreshed cache; the
// synchronous probe here exists only to preserve the fail-fast startup
// contract: a bad/unreachable issuer or JWKS is a startup error, not a
// silent 401 factory discovered on the first request.
func newOIDCVerifier(cfg *config.OIDCConfig) (*oidcVerifier, error) {
	httpClient := &http.Client{
		Timeout: discoveryTimeout,
		Transport: httpx.NewTransport(nil,
			httpx.WithMaxAttempts(oidcProbeAttempts),
			httpx.WithBaseDelay(oidcProbeBackoff)),
	}

	jwksURL, err := discoverAndProbeJWKS(cfg, httpClient)
	if err != nil {
		return nil, err
	}

	keySet := rp.NewRemoteKeySet(httpClient, jwksURL)
	v := rp.NewIDTokenVerifier(cfg.Issuer, cfg.Audience, keySet)
	return &oidcVerifier{issuer: cfg.Issuer, audience: cfg.Audience, verifier: v}, nil
}

// discoverAndProbeJWKS is one attempt at resolving + probing the JWKS
// source; see newOIDCVerifier for the retry loop around it.
func discoverAndProbeJWKS(cfg *config.OIDCConfig, httpClient *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()

	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		doc, err := client.Discover(ctx, cfg.Issuer, httpClient)
		if err != nil {
			return "", fmt.Errorf("oidc discovery for issuer %q: %w", cfg.Issuer, err)
		}
		if doc.JwksURI == "" {
			return "", fmt.Errorf("oidc discovery for issuer %q: response has no jwks_uri", cfg.Issuer)
		}
		jwksURL = doc.JwksURI
	}
	if err := probeJWKS(ctx, httpClient, jwksURL); err != nil {
		return "", fmt.Errorf("fetch jwks from %q: %w", jwksURL, err)
	}
	return jwksURL, nil
}

// probeJWKS performs one synchronous GET of jwksURL, confirming it's
// reachable and that every published key carries the fields a verifier
// actually needs: kty always (RFC 7517), plus kid or alg to pick the right
// key/algorithm for a token's header. A JWKS that decodes but is missing
// these would otherwise pass this probe and only fail later, on the first
// real request. The result is otherwise discarded - actual key resolution at
// verify time goes through rp.NewRemoteKeySet's own lazy, cached fetch.
func probeJWKS(ctx context.Context, httpClient *http.Client, jwksURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(doc.Keys) == 0 {
		return fmt.Errorf("no keys published")
	}
	for i, k := range doc.Keys {
		if k.Kty == "" {
			return fmt.Errorf("key %d: missing required field kty", i)
		}
		if k.Kid == "" && k.Alg == "" {
			return fmt.Errorf("key %d (kty %s): missing both kid and alg - can't be matched to a token's key/algorithm", i, k.Kty)
		}
	}
	return nil
}

// verifyRequest extracts and verifies r's bearer token, returning the caller
// identity from its claims.
func (v *oidcVerifier) verifyRequest(r *http.Request) (Identity, error) {
	tok := bearerToken(r)
	if tok == "" {
		return Identity{}, fmt.Errorf("missing bearer token")
	}
	return v.verify(r.Context(), tok)
}

// verify checks signature, issuer, audience, and expiry via
// rp.VerifyIDToken, then reads the caller identity off the claims:
// preferred_username (falling back to sub) and an optional groups claim.
func (v *oidcVerifier) verify(ctx context.Context, tokenString string) (Identity, error) {
	claims, err := rp.VerifyIDToken[*oidc.IDTokenClaims](ctx, tokenString, v.verifier)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid token: %w", err)
	}
	id := Identity{User: claims.Subject}
	if claims.PreferredUsername != "" {
		id.User = claims.PreferredUsername
	}
	if raw, ok := claims.Claims["groups"]; ok {
		id.Groups = toStringSlice(raw)
	}
	return id, nil
}

// toStringSlice converts a JSON-decoded claim value (a []interface{} of
// strings, per encoding/json's default unmarshal into a map[string]any) into
// []string, skipping non-string entries.
func toStringSlice(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

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

// discoveryTimeout bounds the whole startup probe (discovery + JWKS, including transport retries)
// so a hung IdP can't hang boot.
const discoveryTimeout = 10 * time.Second

// oidcProbeAttempts/oidcProbeBackoff let a transient IdP blip retry during the startup probe; once exhausted
// startup still fails, never falling back to open. oidcProbeBackoff is a var so tests skip real backoff.
const oidcProbeAttempts = 3

var oidcProbeBackoff = time.Second

// oidcVerifier verifies a bearer token's signature, issuer, audience and expiry with zitadel/oidc's
// standalone verifier primitives: a resource server needs only issuer, audience and JWKS.
type oidcVerifier struct {
	issuer   string
	audience string
	verifier *rp.IDTokenVerifier
}

// newOIDCVerifier resolves the JWKS (jwks_url, else discovery, which rejects an issuer mismatch) and probes it
// synchronously so a bad issuer fails startup rather than 401ing every request; the key set itself fetches lazily.
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

// probeJWKS GETs jwksURL once and checks every key has kty plus kid or alg, so a JWKS that decodes
// but can't select keys fails at startup, not on the first request. The result is discarded.
func probeJWKS(ctx context.Context, httpClient *http.Client, jwksURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
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

// verify checks the token via rp.VerifyIDToken, then reads the identity: preferred_username
// (else sub) and an optional groups claim.
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

// toStringSlice converts a JSON-decoded []any claim to []string, skipping non-string entries.
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

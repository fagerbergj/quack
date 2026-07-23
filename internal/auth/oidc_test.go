package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"

	"github.com/fagerbergj/quack/internal/config"
)

// oidcProbeBackoff shrunk for the whole package: these tests care about
// retry *behavior*, not real backoff timing.
func init() { oidcProbeBackoff = time.Millisecond }

// testIdP is a fake OIDC provider: serves discovery + JWKS over httptest, and
// signs tokens with the key it advertises.
type testIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	idp := &testIdP{key: key, kid: "test-kid"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   idp.srv.URL,
			"jwks_uri": idp.srv.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       key.Public(),
			KeyID:     idp.kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

// token mints an RS256 token signed with idp's key. ttl<=0 means "expired 1
// hour ago", exercising the expiry check.
func (idp *testIdP) token(t *testing.T, audience string, ttl time.Duration, extra jwt.MapClaims) string {
	t.Helper()
	now := time.Now()
	exp := now.Add(ttl)
	claims := jwt.MapClaims{
		"iss": idp.srv.URL,
		"aud": audience,
		"sub": "user-123",
		"exp": jwt.NewNumericDate(exp).Unix(),
		"iat": jwt.NewNumericDate(now).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = idp.kid
	s, err := tok.SignedString(idp.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func TestNewOIDCVerifierDiscovery(t *testing.T) {
	idp := newTestIdP(t)
	v, err := newOIDCVerifier(&config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"})
	if err != nil {
		t.Fatalf("newOIDCVerifier: %v", err)
	}
	if v.issuer != idp.srv.URL || v.audience != "quack" {
		t.Errorf("verifier = %+v", v)
	}
}

func TestNewOIDCVerifierBadIssuerFailsLoudly(t *testing.T) {
	_, err := newOIDCVerifier(&config.OIDCConfig{Issuer: "http://127.0.0.1:1", Audience: "quack"})
	if err == nil {
		t.Fatal("expected error for unreachable issuer")
	}
}

// jwksServer serves a JWKS whose raw key objects are exactly keys (bypassing
// jose.JSONWebKeySet's own marshaling, so a test can hand it a structurally
// broken key) plus a matching discovery document.
func jwksServer(t *testing.T, keys []map[string]any) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeJWKSRejectsKeyMissingKty pins review suggestion #1: a JWKS that
// decodes fine but whose key has no "kty" is a structurally broken IdP
// response, and must fail the startup probe rather than pass it silently.
func TestProbeJWKSRejectsKeyMissingKty(t *testing.T) {
	srv := jwksServer(t, []map[string]any{{"kid": "k1", "alg": "RS256"}})
	_, err := newOIDCVerifier(&config.OIDCConfig{Issuer: srv.URL, Audience: "quack"})
	if err == nil {
		t.Fatal("expected error for a key missing kty")
	}
}

// TestProbeJWKSRejectsKeyMissingKidAndAlg: a key with a kty but neither kid
// nor alg can't be matched to a token's header - also structurally broken.
func TestProbeJWKSRejectsKeyMissingKidAndAlg(t *testing.T) {
	srv := jwksServer(t, []map[string]any{{"kty": "RSA"}})
	_, err := newOIDCVerifier(&config.OIDCConfig{Issuer: srv.URL, Audience: "quack"})
	if err == nil {
		t.Fatal("expected error for a key missing both kid and alg")
	}
}

// TestProbeJWKSAcceptsKidOnly: kid alone (no alg) is a legal, common JWKS
// shape and must still pass.
func TestProbeJWKSAcceptsKidOnly(t *testing.T) {
	srv := jwksServer(t, []map[string]any{{"kty": "RSA", "kid": "k1"}})
	if _, err := newOIDCVerifier(&config.OIDCConfig{Issuer: srv.URL, Audience: "quack"}); err != nil {
		t.Fatalf("newOIDCVerifier: %v", err)
	}
}

// TestNewOIDCVerifierRetriesTransientFailure pins review suggestion #2: the
// first couple of probe attempts hitting a 500 (a rolling-deploy blip) don't
// fail startup outright - a later attempt that succeeds does.
func TestNewOIDCVerifierRetriesTransientFailure(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var failures atomic.Int32
	failures.Store(2) // fail the first 2 attempts, succeed on the 3rd (oidcProbeAttempts)

	var flaky *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if failures.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   flaky.URL,
			"jwks_uri": flaky.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "k1", Algorithm: string(jose.RS256), Use: "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	flaky = httptest.NewServer(mux)
	t.Cleanup(flaky.Close)

	v, err := newOIDCVerifier(&config.OIDCConfig{Issuer: flaky.URL, Audience: "quack"})
	if err != nil {
		t.Fatalf("newOIDCVerifier: %v, want it to succeed after retrying past the transient failures", err)
	}
	if v.issuer != flaky.URL {
		t.Errorf("issuer = %q, want %q", v.issuer, flaky.URL)
	}
}

// TestNewOIDCVerifierFailsAfterExhaustingRetries confirms the fail-fast
// contract survives the retry: an IdP that never recovers still fails
// startup, it just takes oidcProbeAttempts tries to say so.
func TestNewOIDCVerifierFailsAfterExhaustingRetries(t *testing.T) {
	mux := http.NewServeMux()
	var hits atomic.Int32
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	dead := httptest.NewServer(mux)
	t.Cleanup(dead.Close)

	_, err := newOIDCVerifier(&config.OIDCConfig{Issuer: dead.URL, Audience: "quack"})
	if err == nil {
		t.Fatal("expected an error once retries are exhausted")
	}
	if got := hits.Load(); got != oidcProbeAttempts {
		t.Errorf("discovery endpoint hit %d times, want exactly oidcProbeAttempts (%d)", got, oidcProbeAttempts)
	}
}

func TestOIDCVerifyToken(t *testing.T) {
	idp := newTestIdP(t)
	v, err := newOIDCVerifier(&config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"})
	if err != nil {
		t.Fatalf("newOIDCVerifier: %v", err)
	}

	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{
			name:  "valid token",
			token: idp.token(t, "quack", time.Hour, nil),
		},
		{
			name:    "expired token",
			token:   idp.token(t, "quack", -time.Hour, nil),
			wantErr: true,
		},
		{
			name:    "wrong audience",
			token:   idp.token(t, "someone-else", time.Hour, nil),
			wantErr: true,
		},
		{
			name:    "malformed token",
			token:   "not-a-jwt",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.verify(t.Context(), tt.token)
			if (err != nil) != tt.wantErr {
				t.Errorf("verify() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestOIDCVerifyExtractsIdentity(t *testing.T) {
	idp := newTestIdP(t)
	v, err := newOIDCVerifier(&config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"})
	if err != nil {
		t.Fatalf("newOIDCVerifier: %v", err)
	}
	tok := idp.token(t, "quack", time.Hour, jwt.MapClaims{
		"preferred_username": "jason",
		"groups":             []any{"admins", "devs"},
	})
	id, err := v.verify(t.Context(), tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.User != "jason" {
		t.Errorf("User = %q, want jason", id.User)
	}
	if len(id.Groups) != 2 || id.Groups[0] != "admins" || id.Groups[1] != "devs" {
		t.Errorf("Groups = %v", id.Groups)
	}
}

func TestOIDCVerifySignedByWrongKeyFails(t *testing.T) {
	idp := newTestIdP(t)
	v, err := newOIDCVerifier(&config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"})
	if err != nil {
		t.Fatalf("newOIDCVerifier: %v", err)
	}
	// Sign with an unrelated key never published in the JWKS - simulates a
	// forged token.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	claims := jwt.MapClaims{
		"iss": idp.srv.URL, "aud": "quack", "sub": "attacker",
		"exp": jwt.NewNumericDate(time.Now().Add(time.Hour)).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = idp.kid
	s, err := tok.SignedString(otherKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.verify(t.Context(), s); err == nil {
		t.Fatal("expected error for a token signed by an unpublished key")
	}
}

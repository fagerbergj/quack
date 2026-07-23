package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"

	"github.com/fagerbergj/quack/internal/config"
)

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

package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKeyPEM generates a throwaway RSA key and returns its PKCS1 PEM plus the
// public key for verifying the App JWT.
func testKeyPEM(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(pemBytes), &key.PublicKey
}

func TestAppJWTClaims(t *testing.T) {
	keyPEM, pub := testKeyPEM(t)
	app, err := NewApp(424242, keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	tokStr, err := app.appJWT()
	if err != nil {
		t.Fatalf("appJWT: %v", err)
	}
	var claims jwt.RegisteredClaims
	_, err = jwt.ParseWithClaims(tokStr, &claims, func(*jwt.Token) (any, error) { return pub, nil })
	if err != nil {
		t.Fatalf("parse jwt: %v", err)
	}
	if claims.Issuer != "424242" {
		t.Errorf("iss = %q; want 424242", claims.Issuer)
	}
	// exp must be in the future and at most 10 minutes out (GitHub's cap).
	d := time.Until(claims.ExpiresAt.Time)
	if d <= 0 || d > 10*time.Minute {
		t.Errorf("exp in %s; want (0, 10m]", d)
	}
	// iat backdated to tolerate clock skew.
	if !claims.IssuedAt.Time.Before(time.Now()) {
		t.Errorf("iat = %v; want backdated", claims.IssuedAt.Time)
	}
}

func TestNewAppRejectsBadKey(t *testing.T) {
	if _, err := NewApp(1, "not a pem"); err == nil {
		t.Fatal("expected error for a bad private key")
	}
}

func TestInstallationTokenCaching(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/99/access_tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		atomic.AddInt32(&hits, 1)
		fmt.Fprintf(w, `{"token":"ghs_secret","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	}))
	defer srv.Close()

	app, err := NewApp(1, keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL

	for i := 0; i < 3; i++ {
		tok, err := app.InstallationToken(context.Background(), 99)
		if err != nil {
			t.Fatalf("InstallationToken: %v", err)
		}
		if tok != "ghs_secret" {
			t.Fatalf("token = %q; want ghs_secret", tok)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("token endpoint hit %d times; want 1 (cached)", got)
	}
}

func TestInstallationForRepoCaching(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/installation" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, `{"id":777}`)
	}))
	defer srv.Close()

	app, err := NewApp(1, keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL

	for i := 0; i < 2; i++ {
		id, err := app.InstallationForRepo(context.Background(), "acme", "widgets")
		if err != nil {
			t.Fatalf("InstallationForRepo: %v", err)
		}
		if id != 777 {
			t.Fatalf("id = %d; want 777", id)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("installation endpoint hit %d times; want 1 (cached)", got)
	}
}

func TestOwnerRepoFromURL(t *testing.T) {
	tests := []struct {
		url         string
		owner, repo string
		ok          bool
	}{
		{"https://github.com/acme/widgets.git", "acme", "widgets", true},
		{"https://github.com/acme/widgets", "acme", "widgets", true},
		{"https://gitlab.com/acme/widgets.git", "", "", false},
		{"https://github.com/acme", "", "", false},
		{"not a url ::", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			owner, repo, ok := ownerRepoFromURL(tt.url)
			if ok != tt.ok || owner != tt.owner || repo != tt.repo {
				t.Errorf("got (%q,%q,%v); want (%q,%q,%v)", owner, repo, ok, tt.owner, tt.repo, tt.ok)
			}
		})
	}
}

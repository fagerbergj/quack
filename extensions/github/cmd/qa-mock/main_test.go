package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	gh "github.com/fagerbergj/quack-extensions/github"
)

func testPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: b}))
}

// Exercises the real App client (InstallationForRepo, InstallationToken) against this mock's handlers.
func TestMockServesRealClient(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "get"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &server{dir: dir, deliveries: mustDeliveries(t, dir)}
	ts := httptest.NewServer(s)
	defer ts.Close()

	req := httptest.NewRequest("GET", "/repos/acme/widgets/installation", nil)
	fixturePath := filepath.Join(dir, "get", fixtureKey(req))
	if err := os.WriteFile(fixturePath, []byte(`{"id":42}`), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := gh.NewApp("Iv23liTest", testPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	app.SetAPIBase(ts.URL)

	id, err := app.InstallationForRepo(context.Background(), "acme", "widgets")
	if err != nil {
		t.Fatalf("InstallationForRepo: %v", err)
	}
	if id != 42 {
		t.Fatalf("installation id = %d, want 42", id)
	}

	tok, err := app.InstallationToken(context.Background(), id)
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if tok != "qa-mock-token" {
		t.Fatalf("token = %q, want qa-mock-token", tok)
	}
}

func mustDeliveries(t *testing.T, dir string) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "deliveries.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

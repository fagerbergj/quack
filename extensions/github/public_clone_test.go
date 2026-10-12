package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A public repo the App isn't installed on must clone anonymously: GitCredential returns no credential and no
// error. An installation token for another account makes GitHub 404 repos it cannot see.
func TestGitCredential_PublicRepoWithoutInstallation_ClonesAnonymously(t *testing.T) {
	// GitHub answers 404 on /repos/{owner}/{repo}/installation when the App is not
	// installed there.
	var installCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		installCalls++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	app := newTestApp(t, srv.URL)

	cred, err := app.GitCredential(context.Background(), "https://github.com/All-Hands-AI/OpenHands")
	if err != nil {
		t.Fatalf("GitCredential returned an error for a public repo: %v\nthis is the bug: no installation must mean NO credential, not a failed clone", err)
	}
	if cred != nil {
		t.Fatalf("GitCredential returned a credential %+v for a repo the App is not installed on; want nil so git clones anonymously", cred)
	}

	// The negative result is cached: a second clone of the same repo must not re-ask
	// GitHub (that round-trip on every clone is pure churn).
	if _, err := app.GitCredential(context.Background(), "https://github.com/All-Hands-AI/OpenHands"); err != nil {
		t.Fatalf("second GitCredential: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("hit the installation endpoint %d times; want 1 (the miss must be cached)", installCalls)
	}
}

// InstallationForRepo reports a missing installation as ErrNoInstallation so callers
// can distinguish "not installed" (fall back to anonymous) from a real API failure.
func TestInstallationForRepo_NotInstalled_IsErrNoInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	app := newTestApp(t, srv.URL)

	_, err := app.InstallationForRepo(context.Background(), "block", "goose")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("got %v; want ErrNoInstallation so the caller can clone anonymously", err)
	}
}

// A non-github host is untouched: no credential, no error.
func TestGitCredential_NonGitHubHost_NoCredential(t *testing.T) {
	app := newTestApp(t, "http://unused.invalid")
	cred, err := app.GitCredential(context.Background(), "https://gitlab.com/acme/widgets")
	if err != nil || cred != nil {
		t.Fatalf("got (%+v, %v); want (nil, nil) for a non-github host", cred, err)
	}
}

// newTestApp builds an App whose REST calls go to the stub server.
func newTestApp(t *testing.T, apiBase string) *App {
	t.Helper()
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("Iv23liTestClientId", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = apiBase
	return app
}

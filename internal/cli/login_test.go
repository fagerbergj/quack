package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// fakeOP is a minimal OIDC provider for the CLI login/refresh tests: serves
// discovery, an authorize endpoint that enforces PKCE, and a token endpoint
// that mints a fresh access/refresh token pair per grant type.
type fakeOP struct {
	srv *httptest.Server

	mu             sync.Mutex
	accessTokenSeq int // incremented per token issued, so refreshed tokens are distinguishable
	codeSeq        int
	challenges     map[string]oidc.CodeChallenge // authorization code -> the PKCE challenge it was requested with
	pkceVerified   bool                          // set once a token exchange has actually checked a code_verifier
}

func newFakeOP(t *testing.T) *fakeOP {
	t.Helper()
	op := &fakeOP{challenges: map[string]oidc.CodeChallenge{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 op.srv.URL,
			"token_endpoint":         op.srv.URL + "/token",
			"authorization_endpoint": op.srv.URL + "/authorize",
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirectURI := q.Get("redirect_uri")

		op.mu.Lock()
		op.codeSeq++
		code := fmt.Sprintf("auth-code-%d", op.codeSeq)
		op.challenges[code] = oidc.CodeChallenge{
			Challenge: q.Get("code_challenge"),
			Method:    oidc.CodeChallengeMethod(q.Get("code_challenge_method")),
		}
		op.mu.Unlock()

		u, err := url.Parse(redirectURI)
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		v := u.Query()
		v.Set("code", code)
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("grant_type") {
		case string(oidc.GrantTypeCode):
			code := r.FormValue("code")
			op.mu.Lock()
			challenge, ok := op.challenges[code]
			op.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(oidc.Error{ErrorType: "invalid_grant", Description: "unknown code"})
				return
			}
			if !oidc.VerifyCodeChallenge(&challenge, r.FormValue("code_verifier")) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(oidc.Error{ErrorType: "invalid_grant", Description: "PKCE verification failed"})
				return
			}
			op.mu.Lock()
			op.pkceVerified = true
			op.mu.Unlock()
			op.issueToken(w)
		case string(oidc.GrantTypeRefreshToken):
			op.issueToken(w)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(oidc.Error{ErrorType: "unsupported_grant_type"})
		}
	})
	op.srv = httptest.NewServer(mux)
	t.Cleanup(op.srv.Close)
	return op
}

// issueToken mints an access/refresh token pair with no id_token - Login
// tolerates rp.ErrMissingIDToken from a token response like this one, the
// same way most fake/minimal OPs in the wild behave for a client that didn't
// strictly require one.
func (op *fakeOP) issueToken(w http.ResponseWriter) {
	op.mu.Lock()
	op.accessTokenSeq++
	seq := op.accessTokenSeq
	op.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(oidc.AccessTokenResponse{
		AccessToken:  fmt.Sprintf("access-token-%d", seq),
		RefreshToken: fmt.Sprintf("refresh-token-%d", seq),
		TokenType:    "Bearer",
		ExpiresIn:    3600,
	})
}

// simulateBrowser plays the user's browser: GETs authURL (not following the
// redirect, so the Location can be inspected), then GETs that Location -
// quack's own loopback callback - completing the round trip synchronously.
func simulateBrowser(t *testing.T, authURL string) {
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(authURL)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("authorize: no Location header on the redirect")
	}
	cbResp, err := http.Get(loc)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	defer cbResp.Body.Close()
}

// TestLoginAuthCodePKCE drives the full authorization code + PKCE flow
// against a fake OP (via a stubbed openBrowser simulating the user) and
// confirms the resulting tokens land on the registered server, and that the
// fake OP's token endpoint actually verified the code_verifier against the
// code_challenge from the authorize step - the PKCE round trip is real, not
// just plumbed through.
func TestLoginAuthCodePKCE(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	op := newFakeOP(t)

	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("myserver", "https://quack.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := cc.Save(); err != nil {
		t.Fatal(err)
	}

	origOpenBrowser := openBrowser
	defer func() { openBrowser = origOpenBrowser }()
	openBrowser = func(authURL string) { simulateBrowser(t, authURL) }

	var out bytes.Buffer
	if err := Login(context.Background(), &out, "myserver", op.srv.URL, "cli-client", nil); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !strings.Contains(out.String(), "logged in to myserver") {
		t.Errorf("output = %q, want a success line", out.String())
	}

	op.mu.Lock()
	verified := op.pkceVerified
	op.mu.Unlock()
	if !verified {
		t.Error("token endpoint never checked a code_verifier - PKCE round trip did not happen")
	}

	reloaded, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := reloaded.Servers["myserver"]
	if !ok || ref.Auth == nil {
		t.Fatalf("server %+v has no stored auth", ref)
	}
	if ref.Auth.AccessToken != "access-token-1" {
		t.Errorf("AccessToken = %q, want access-token-1", ref.Auth.AccessToken)
	}
	if ref.Auth.RefreshToken != "refresh-token-1" {
		t.Errorf("RefreshToken = %q, want refresh-token-1", ref.Auth.RefreshToken)
	}
	if ref.Auth.Issuer != op.srv.URL {
		t.Errorf("Issuer = %q, want %q", ref.Auth.Issuer, op.srv.URL)
	}
	if ref.Auth.ClientID != "cli-client" {
		t.Errorf("ClientID = %q, want cli-client", ref.Auth.ClientID)
	}
	if ref.Auth.TokenURL != op.srv.URL+"/token" {
		t.Errorf("TokenURL = %q, want %s/token", ref.Auth.TokenURL, op.srv.URL)
	}
	if ref.Auth.Expiry.Before(time.Now().Add(time.Hour - time.Minute)) {
		t.Errorf("Expiry = %v, want ~1h from now", ref.Auth.Expiry)
	}
}

// TestLoginStateMismatchRejected drives a callback whose state doesn't match
// what Login generated (a confused-deputy/CSRF redirect) and confirms Login
// rejects it and stores nothing.
func TestLoginStateMismatchRejected(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	op := newFakeOP(t)

	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("myserver", "https://quack.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := cc.Save(); err != nil {
		t.Fatal(err)
	}

	origOpenBrowser := openBrowser
	defer func() { openBrowser = origOpenBrowser }()
	openBrowser = func(authURL string) {
		u, err := url.Parse(authURL)
		if err != nil {
			t.Fatalf("parse authURL: %v", err)
		}
		cbURL := u.Query().Get("redirect_uri") + "?code=whatever&state=not-the-real-state"
		resp, err := http.Get(cbURL)
		if err != nil {
			t.Fatalf("GET callback: %v", err)
		}
		defer resp.Body.Close()
	}

	var out bytes.Buffer
	err = Login(context.Background(), &out, "myserver", op.srv.URL, "cli-client", nil)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("Login with a mismatched callback state: err = %v, want a state-mismatch error", err)
	}

	reloaded, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Servers["myserver"].Auth != nil {
		t.Errorf("Auth = %+v, want nil - login should not have completed", reloaded.Servers["myserver"].Auth)
	}
}

// TestLoginUnregisteredServer requires `server add` first, same as
// `server use`. Fails before ever touching the network, so no fake OP needed.
func TestLoginUnregisteredServer(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var out bytes.Buffer
	err := Login(context.Background(), &out, "ghost", "https://issuer.example.com", "client-id", nil)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("Login on an unregistered server: err = %v, want a not-registered error", err)
	}
}

// TestEnsureFreshTokenNoAuth: a server with no stored session attaches
// nothing, and never touches the network.
func TestEnsureFreshTokenNoAuth(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ensureFreshToken(context.Background(), cc, "s", ServerRef{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("ensureFreshToken: %v", err)
	}
	if tok != "" {
		t.Errorf("token = %q, want empty (no auth configured)", tok)
	}
}

// TestEnsureFreshTokenNotExpiredSkipsRefresh proves a token well inside its
// expiry is returned as-is - the fake OP's token endpoint would fail this
// test if hit, since ParseForm on an empty POST won't match any grant type
// and returns 400.
func TestEnsureFreshTokenNotExpiredSkipsRefresh(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	op := newFakeOP(t)
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("s", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	ref := ServerRef{URL: "https://example.com", Auth: &ServerAuth{
		ClientID:     "c",
		TokenURL:     op.srv.URL + "/token",
		AccessToken:  "still-good",
		RefreshToken: "r",
		Expiry:       time.Now().Add(time.Hour),
	}}
	tok, err := ensureFreshToken(context.Background(), cc, "s", ref)
	if err != nil {
		t.Fatalf("ensureFreshToken: %v", err)
	}
	if tok != "still-good" {
		t.Errorf("token = %q, want still-good (unchanged)", tok)
	}
}

// TestEnsureFreshTokenRefreshesNearExpiry proves a token within the skew
// window is refreshed via the token endpoint, and the new tokens are
// persisted back to the registry.
func TestEnsureFreshTokenRefreshesNearExpiry(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	op := newFakeOP(t)
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("s", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if err := cc.Save(); err != nil {
		t.Fatal(err)
	}
	ref := ServerRef{URL: "https://example.com", Auth: &ServerAuth{
		ClientID:     "c",
		TokenURL:     op.srv.URL + "/token",
		AccessToken:  "about-to-expire",
		RefreshToken: "refresh-0",
		Expiry:       time.Now().Add(time.Second),
	}}
	tok, err := ensureFreshToken(context.Background(), cc, "s", ref)
	if err != nil {
		t.Fatalf("ensureFreshToken: %v", err)
	}
	if tok != "access-token-1" {
		t.Errorf("token = %q, want the refreshed access-token-1", tok)
	}

	reloaded, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Servers["s"].Auth
	if got == nil || got.AccessToken != "access-token-1" || got.RefreshToken != "refresh-token-1" {
		t.Errorf("persisted auth = %+v, want the refreshed tokens", got)
	}
}

// TestEnsureFreshTokenRefreshSurvivesCallerCancellation pins the fix for the
// finding that a refresh must not be bound to the caller's own context: the
// ctx passed in is already cancelled (as if the request that triggered the
// refresh was aborted), but the refresh - detached via context.WithoutCancel
// - still completes and persists.
func TestEnsureFreshTokenRefreshSurvivesCallerCancellation(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	op := newFakeOP(t)
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("s", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if err := cc.Save(); err != nil {
		t.Fatal(err)
	}
	ref := ServerRef{URL: "https://example.com", Auth: &ServerAuth{
		ClientID:     "c",
		TokenURL:     op.srv.URL + "/token",
		AccessToken:  "about-to-expire",
		RefreshToken: "refresh-0",
		Expiry:       time.Now().Add(time.Second),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller's context is already cancelled before ensureFreshToken runs

	tok, err := ensureFreshToken(ctx, cc, "s", ref)
	if err != nil {
		t.Fatalf("ensureFreshToken with a cancelled caller ctx: %v", err)
	}
	if tok != "access-token-1" {
		t.Errorf("token = %q, want the refreshed access-token-1", tok)
	}

	reloaded, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Servers["s"].Auth
	if got == nil || got.AccessToken != "access-token-1" {
		t.Errorf("persisted auth = %+v, want the refreshed token", got)
	}
}

// TestEnsureFreshTokenNoRefreshTokenReturnsAsIs: an expired token with no
// refresh token to use is handed back unchanged - the server 401s it, which
// is the caller's signal to `server login` again.
func TestEnsureFreshTokenNoRefreshTokenReturnsAsIs(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	ref := ServerRef{URL: "https://example.com", Auth: &ServerAuth{
		AccessToken: "expired",
		Expiry:      time.Now().Add(-time.Hour),
	}}
	tok, err := ensureFreshToken(context.Background(), cc, "s", ref)
	if err != nil {
		t.Fatalf("ensureFreshToken: %v", err)
	}
	if tok != "expired" {
		t.Errorf("token = %q, want expired (returned as-is, nothing to refresh with)", tok)
	}
}

// TestEnsureFreshTokenConcurrentRefreshCoalesces pins review suggestion #3:
// several goroutines racing the same near-expiry token (e.g. the TUI firing
// off more than one client call at once) must coalesce into exactly one
// refresh, with every caller observing the same resulting token - not each
// independently hitting the token endpoint and risking a rotating-refresh
// IdP invalidating one of them out from under the other.
func TestEnsureFreshTokenConcurrentRefreshCoalesces(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	op := newFakeOP(t)
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("s", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if err := cc.Save(); err != nil {
		t.Fatal(err)
	}
	ref := ServerRef{URL: "https://example.com", Auth: &ServerAuth{
		ClientID:     "c",
		TokenURL:     op.srv.URL + "/token",
		AccessToken:  "about-to-expire",
		RefreshToken: "refresh-0",
		Expiry:       time.Now().Add(time.Second),
	}}

	const n = 8
	var wg sync.WaitGroup
	tokens := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = ensureFreshToken(context.Background(), cc, "s", ref)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: ensureFreshToken: %v", i, err)
		}
	}
	want := tokens[0]
	for i, got := range tokens {
		if got != want {
			t.Errorf("goroutine %d token = %q, want %q (every caller should observe the same refreshed token)", i, got, want)
		}
	}
	if op.accessTokenSeq != 1 {
		t.Errorf("token endpoint hit %d times, want exactly 1 (concurrent refreshes should coalesce)", op.accessTokenSeq)
	}
}

// TestNewClientAttachesBearerToken is the end-to-end path: a registered
// server with a stored session gets its access token attached as a bearer
// credential on every request NewClient's Client makes.
func TestNewClientAttachesBearerToken(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())

	var gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer api.Close()

	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("myserver", api.URL); err != nil {
		t.Fatal(err)
	}
	if err := cc.Use("myserver"); err != nil {
		t.Fatal(err)
	}
	if err := cc.SetAuth("myserver", &ServerAuth{AccessToken: "secret-token"}); err != nil {
		t.Fatal(err)
	}
	if err := cc.Save(); err != nil {
		t.Fatal(err)
	}

	c, err := NewClient(context.Background(), "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.ListChats(context.Background()); err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-token")
	}
}

// TestNewClientNoAuthConfiguredAttachesNothing is the unchanged path: a
// server with no stored session sends no Authorization header at all -
// today's open behavior for a server with no auth configured.
func TestNewClientNoAuthConfiguredAttachesNothing(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())

	var gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer api.Close()

	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("myserver", api.URL); err != nil {
		t.Fatal(err)
	}
	if err := cc.Use("myserver"); err != nil {
		t.Fatal(err)
	}
	if err := cc.Save(); err != nil {
		t.Fatal(err)
	}

	c, err := NewClient(context.Background(), "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.ListChats(context.Background()); err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want none", gotAuth)
	}
}

// TestSaveIsPrivate confirms the registry file (and its directory) are
// written without group/other permissions - the cheap mitigation for a file
// that can hold OIDC access/refresh tokens once `server login` has run.
func TestSaveIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on windows")
	}
	// A not-yet-existing dir so Save's os.MkdirAll(..., 0o700) is actually
	// exercised, rather than inheriting t.TempDir()'s own 0700.
	dir := filepath.Join(t.TempDir(), "quackhome")
	t.Setenv("QUACK_HOME", dir)

	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.AddServer("s", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if err := cc.Save(); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(dir, "servers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("servers.yaml mode = %v, want no group/other bits", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm()&0o077 != 0 {
		t.Errorf("%s mode = %v, want no group/other bits", dir, di.Mode().Perm())
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// fakeOP is a minimal OIDC provider for the CLI login/refresh tests: serves
// discovery, the device authorization endpoint, and a token endpoint that
// mints a fresh access/refresh token pair per grant type. deviceInterval
// controls the "interval" field in the device authorization response (0 lets
// the caller's own fallback/deviceAuthPollFloor apply).
type fakeOP struct {
	srv            *httptest.Server
	deviceInterval int
	accessTokenSeq int // incremented per token issued, so refreshed tokens are distinguishable
}

func newFakeOP(t *testing.T) *fakeOP {
	t.Helper()
	op := &fakeOP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                        op.srv.URL,
			"token_endpoint":                op.srv.URL + "/token",
			"device_authorization_endpoint": op.srv.URL + "/device_authorization",
		})
	})
	mux.HandleFunc("/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oidc.DeviceAuthorizationResponse{
			DeviceCode:              "dev-code-1",
			UserCode:                "ABCD-1234",
			VerificationURI:         op.srv.URL + "/device",
			VerificationURIComplete: op.srv.URL + "/device?user_code=ABCD-1234",
			ExpiresIn:               600,
			Interval:                op.deviceInterval,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.FormValue("grant_type") {
		case string(oidc.GrantTypeDeviceCode), string(oidc.GrantTypeRefreshToken):
			op.accessTokenSeq++
			seq := strconv.Itoa(op.accessTokenSeq)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(oidc.AccessTokenResponse{
				AccessToken:  "access-token-" + seq,
				RefreshToken: "refresh-token-" + seq,
				TokenType:    "Bearer",
				ExpiresIn:    3600,
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(oidc.Error{ErrorType: "unsupported_grant_type"})
		}
	})
	op.srv = httptest.NewServer(mux)
	t.Cleanup(op.srv.Close)
	return op
}

// TestLoginDeviceFlow drives the full device authorization grant against a
// fake OP and confirms the resulting tokens land on the registered server.
func TestLoginDeviceFlow(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	deviceAuthPollFloor = time.Millisecond
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

	var out bytes.Buffer
	if err := Login(context.Background(), &out, "myserver", op.srv.URL, "cli-client", nil); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !strings.Contains(out.String(), "ABCD-1234") {
		t.Errorf("output = %q, want it to mention the user code", out.String())
	}
	if !strings.Contains(out.String(), "logged in to myserver") {
		t.Errorf("output = %q, want a success line", out.String())
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

// TestLoginUnregisteredServer requires `server add` first, same as
// `server use`.
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
// test if hit, since ParseForm on an empty POST won't match either grant type
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

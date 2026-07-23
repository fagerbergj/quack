package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/zitadel/oidc/v3/pkg/client/rp"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"golang.org/x/oauth2"
)

// DefaultLoginScopes is requested when `server login` is given none: openid
// (required for a sub/preferred_username-bearing token), profile
// (preferred_username), and offline_access (a refresh token - several IdPs,
// e.g. Keycloak and Authentik, only issue one when it's explicitly asked for).
var DefaultLoginScopes = []string{"openid", "profile", "offline_access"}

// expirySkew triggers a proactive token refresh shortly before real expiry,
// so a request in flight doesn't race a token that's about to lapse.
const expirySkew = 30 * time.Second

// refreshTimeout bounds a token refresh detached from the caller's own
// context (see ensureFreshToken) - long enough for a slow IdP, short enough
// that a dead token endpoint doesn't hang the caller indefinitely.
const refreshTimeout = 15 * time.Second

// loginCallbackTimeout bounds how long Login waits for the browser round trip
// after opening the authorize URL, so an abandoned login doesn't hang the CLI
// forever. A var so tests don't have to wait out a real timeout to cover the
// "nobody ever came back" path.
var loginCallbackTimeout = 5 * time.Minute

// openBrowser best-effort launches url in the user's default browser. A var
// so tests can replace it with a synchronous fake-IdP + callback round trip
// (see login_test.go) instead of shelling out. Errors are swallowed - the URL
// printed to out is always the fallback, and a headless box with no
// $DISPLAY/xdg-open shouldn't fail login, just leave the user to copy the link.
var openBrowser = func(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// Login runs the OAuth 2.0 Authorization Code flow with PKCE (RFC 6749 +
// RFC 7636) against issuer for clientID - the RFC 8252 "native app" pattern:
// a loopback listener on an ephemeral port stands in for the redirect URI (no
// port to pre-register with the IdP), the authorize URL is opened in a
// browser (and printed as a fallback), and Login blocks until the redirect
// lands back on the listener. Stores the resulting tokens on the
// already-registered server name so NewClient picks them up automatically.
//
// This replaces an earlier device authorization grant (RFC 8628) implementation.
// Auth code + PKCE is the flow RFC 8252 actually recommends for a CLI/native
// client, but it trades away the device grant's one real advantage: a
// headless/SSH box with no local browser and no port the user's browser can
// reach can't complete this flow the way it could poll through a device code.
//
// Only public OIDC clients are supported (no client secret): PKCE is meant
// for exactly this - a client that can't keep a secret - and it keeps the
// stored registry free of anything more sensitive than the tokens themselves.
func Login(ctx context.Context, out io.Writer, name, issuer, clientID string, scopes []string) error {
	cc, err := LoadClient()
	if err != nil {
		return err
	}
	if _, ok := cc.Servers[name]; !ok {
		return fmt.Errorf("server %q is not registered (run `quack server add` first)", name)
	}
	if len(scopes) == 0 {
		scopes = DefaultLoginScopes
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind loopback callback listener: %w", err)
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	relyingParty, err := rp.NewRelyingPartyOIDC(ctx, issuer, clientID, "", redirectURI, scopes)
	if err != nil {
		listener.Close()
		return fmt.Errorf("oidc discovery for issuer %q: %w", issuer, err)
	}

	state, err := randomURLSafe(32)
	if err != nil {
		listener.Close()
		return fmt.Errorf("generate state: %w", err)
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		listener.Close()
		return fmt.Errorf("generate PKCE verifier: %w", err)
	}
	challenge := oidc.NewSHACodeChallenge(verifier)
	authURL := rp.AuthURL(state, relyingParty, rp.WithCodeChallenge(challenge))

	code, err := awaitCallback(ctx, listener, state, func() {
		fmt.Fprintf(out, "Open %s to log in\n", authURL)
		openBrowser(authURL)
	})
	if err != nil {
		return err
	}

	tokens, err := rp.CodeExchange[*oidc.IDTokenClaims](ctx, code, relyingParty, rp.WithCodeVerifier(verifier))
	if err != nil && !errors.Is(err, rp.ErrMissingIDToken) {
		return fmt.Errorf("code exchange: %w", err)
	}

	auth := &ServerAuth{
		Issuer:       issuer,
		ClientID:     clientID,
		Scopes:       scopes,
		TokenURL:     relyingParty.OAuthConfig().Endpoint.TokenURL,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		Expiry:       tokens.Expiry,
	}
	if err := cc.SetAuth(name, auth); err != nil {
		return err
	}
	if err := cc.Save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "logged in to %s\n", name)
	return nil
}

// randomURLSafe returns n random bytes, base64url-encoded (no padding) - used
// for both the PKCE code verifier (RFC 7636 wants 43-128 chars; 32 bytes
// yields 43) and the CSRF state, sized the same for simplicity.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// awaitCallback serves exactly one /callback request on listener (closing it
// on return either way), checking state on the way in to guard against a
// CSRF/confused-deputy redirect, and returns the authorization code. announce
// is called once the listener is live, so the caller can print the authorize
// URL and open a browser - after which awaitCallback blocks until the
// redirect lands or ctx/loginCallbackTimeout expires.
func awaitCallback(ctx context.Context, listener net.Listener, state string, announce func()) (string, error) {
	type result struct {
		code string
		err  error
	}
	resultCh := make(chan result, 1)
	var once sync.Once
	send := func(r result) { once.Do(func() { resultCh <- r }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("error") != "":
			send(result{err: fmt.Errorf("authorization failed: %s: %s", q.Get("error"), q.Get("error_description"))})
			http.Error(w, "Login failed - you can close this tab and check the CLI.", http.StatusBadRequest)
		case q.Get("state") != state:
			send(result{err: errors.New("callback state did not match - possible CSRF, aborting login")})
			http.Error(w, "Login failed (state mismatch) - you can close this tab and check the CLI.", http.StatusBadRequest)
		case q.Get("code") == "":
			send(result{err: errors.New("callback carried no authorization code")})
			http.Error(w, "Login failed (no code) - you can close this tab and check the CLI.", http.StatusBadRequest)
		default:
			send(result{code: q.Get("code")})
			fmt.Fprint(w, "<p><strong>Logged in.</strong> You can close this tab and return to the CLI.</p>")
		}
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	announce()

	waitCtx, cancelWait := context.WithTimeout(ctx, loginCallbackTimeout)
	defer cancelWait()
	select {
	case res := <-resultCh:
		return res.code, res.err
	case <-waitCtx.Done():
		return "", fmt.Errorf("timed out waiting for the browser login to complete: %w", waitCtx.Err())
	}
}

// ensureFreshToken returns ref's access token, refreshing it first (via the
// OAuth2 token endpoint's refresh_token grant) and persisting the result if
// it's at or near expiry. A ref with no stored auth returns "" (the caller
// attaches no Authorization header - matching an unauthenticated server). A
// ref whose token has no refresh_token and has expired is returned as-is;
// the server will 401 it, which is the caller's signal to `server login`
// again - there is nothing to refresh with.
//
// Uses golang.org/x/oauth2's Config.TokenSource directly (the same
// refresh_token-grant mechanism rp.RelyingParty wraps) rather than
// rp.RefreshTokens, which is generic over oidc.IDClaims for ID-token
// verification this bearer-relaying CLI client has no use for.
//
// refreshMu serializes this across goroutines in one process (e.g. the TUI,
// which can fire several client calls concurrently): without it, two callers
// racing the same near-expiry token can each refresh independently, and an
// IdP that rotates refresh tokens invalidates the first one as soon as the
// second is consumed, failing whichever call loses the race. This does not
// cover two separate `quack` process invocations racing the same file.
func ensureFreshToken(ctx context.Context, cc *ClientConfig, name string, ref ServerRef) (string, error) {
	a := ref.Auth
	if a == nil {
		return "", nil
	}
	if a.RefreshToken == "" || a.Expiry.IsZero() || time.Now().Add(expirySkew).Before(a.Expiry) {
		return a.AccessToken, nil
	}

	refreshMu.Lock()
	defer refreshMu.Unlock()

	// Re-read from disk now that we hold the lock: a concurrent caller may
	// have already refreshed (and persisted) while this one waited.
	if fresh, err := LoadClient(); err == nil {
		if freshRef, ok := fresh.Servers[name]; ok && freshRef.Auth != nil {
			a = freshRef.Auth
			cc = fresh
			if time.Now().Add(expirySkew).Before(a.Expiry) {
				return a.AccessToken, nil
			}
		}
	}

	cfg := &oauth2.Config{
		ClientID: a.ClientID,
		Endpoint: oauth2.Endpoint{TokenURL: a.TokenURL},
		Scopes:   a.Scopes,
	}
	// Detached from the caller's cancellation/deadline: a token refresh must
	// complete (or time out on its own terms) even if the request that
	// triggered it was aborted - otherwise a short-lived caller ctx can poison
	// a refresh that every other in-flight caller also depends on. Values
	// (e.g. request-scoped tracing) still flow through via WithoutCancel.
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	newTok, err := cfg.TokenSource(refreshCtx, &oauth2.Token{
		RefreshToken: a.RefreshToken,
		Expiry:       a.Expiry,
	}).Token()
	if err != nil {
		return "", fmt.Errorf("refresh oidc token for server %q: %w", name, err)
	}

	updated := *a
	updated.AccessToken = newTok.AccessToken
	updated.Expiry = newTok.Expiry
	if newTok.RefreshToken != "" {
		updated.RefreshToken = newTok.RefreshToken
	}
	if err := cc.SetAuth(name, &updated); err == nil {
		_ = cc.Save() // best-effort: a failed write just means the next call refreshes again
	}
	return updated.AccessToken, nil
}

// refreshMu is package-level (not per-ClientConfig): each ensureFreshToken
// call typically works off its own freshly-LoadClient'd instance, so a mutex
// scoped to that struct would never actually be shared between callers.
var refreshMu sync.Mutex

// bearerTransport injects "Authorization: Bearer <token>" into every request
// it forwards - how a Client attaches a stored OIDC session's access token
// without every call site having to know about it.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

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

// DefaultLoginScopes: openid and profile for preferred_username, offline_access for a refresh token
// (Keycloak and Authentik only issue one when asked).
var DefaultLoginScopes = []string{"openid", "profile", "offline_access"}

// expirySkew triggers a proactive token refresh shortly before real expiry,
// so a request in flight doesn't race a token that's about to lapse.
const expirySkew = 30 * time.Second

// refreshTimeout bounds a refresh detached from the caller's context: long enough for a slow IdP,
// short enough that a dead token endpoint doesn't hang the caller.
const refreshTimeout = 15 * time.Second

// loginCallbackTimeout bounds the wait for the browser round trip so an abandoned login doesn't hang.
// A var so tests can shorten it.
var loginCallbackTimeout = 5 * time.Minute

// openBrowser best-effort opens url; a var so tests can fake the IdP round trip. Errors are swallowed:
// the printed URL is the fallback on a headless box.
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

// Login runs the OAuth Authorization Code + PKCE flow with a loopback redirect (RFC 8252) and stores the
// tokens on the registered server. Public clients only, no secret; headless boxes would need RFC 8628.
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
		_ = listener.Close()
		return fmt.Errorf("oidc discovery for issuer %q: %w", issuer, err)
	}

	state, err := randomURLSafe(32)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("generate state: %w", err)
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		_ = listener.Close()
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

// randomURLSafe returns n random bytes base64url-encoded without padding; 32 bytes gives the
// 43-char minimum RFC 7636 wants for a PKCE verifier.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// awaitCallback serves one /callback on listener, checks state against CSRF, and returns the code.
// announce runs once the listener is live, so the caller can print and open the authorize URL.
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
			_, _ = fmt.Fprint(w, "<p><strong>Logged in.</strong> You can close this tab and return to the CLI.</p>")
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

// ensureFreshToken returns ref's access token, refreshing and persisting it if near expiry ("" for no auth).
// refreshMu serializes refreshes in-process; IdPs that rotate refresh tokens break on concurrent refresh.
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
	// Detached from caller cancellation: an aborted request must not poison a refresh that other
	// in-flight callers depend on.
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

// refreshMu is package-level because each caller usually holds its own freshly loaded ClientConfig,
// so a per-struct mutex would never be shared.
var refreshMu sync.Mutex

// bearerTransport adds "Authorization: Bearer <token>" to every request it forwards.
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

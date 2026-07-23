package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/zitadel/oidc/v3/pkg/client/rp"
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

// deviceAuthPollFloor is the device-flow poll interval used when the
// provider's device authorization response omits one (interval <= 0) - a var
// (not a constant) so tests can shrink it, matching client.go's
// sseReconnectDelay pattern.
var deviceAuthPollFloor = 5 * time.Second

// Login runs the OAuth 2.0 Device Authorization Grant (RFC 8628) against
// issuer for clientID, prints the verification URL and user code to out,
// blocks until the user completes it in a browser (any browser, any
// machine), and stores the resulting tokens on the already-registered server
// name so NewClient picks them up automatically from then on.
//
// Device grant over the loopback-redirect alternative (spin up a local HTTP
// server, open a browser, catch the redirect): no port to bind, so it works
// over SSH/headless sessions - the common case for a server CLI - and
// zitadel/oidc's rp.DeviceAuthorization/rp.DeviceAccessToken already
// implement the polling loop (authorization_pending/slow_down backoff) RFC
// 8628 requires, so there is nothing to hand-roll.
//
// Only public OIDC clients are supported (no client secret): device grant is
// meant for exactly this - a client that can't keep a secret - and it keeps
// the stored registry free of anything more sensitive than the tokens
// themselves.
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

	relyingParty, err := rp.NewRelyingPartyOIDC(ctx, issuer, clientID, "", "", scopes)
	if err != nil {
		return fmt.Errorf("oidc discovery for issuer %q: %w", issuer, err)
	}

	authResp, err := rp.DeviceAuthorization(ctx, scopes, relyingParty, nil)
	if err != nil {
		return fmt.Errorf("start device authorization: %w", err)
	}
	if authResp.VerificationURIComplete != "" {
		fmt.Fprintf(out, "Open %s to log in (code: %s)\n", authResp.VerificationURIComplete, authResp.UserCode)
	} else {
		fmt.Fprintf(out, "Open %s and enter code: %s\n", authResp.VerificationURI, authResp.UserCode)
	}

	interval := time.Duration(authResp.Interval) * time.Second
	if interval <= 0 {
		interval = deviceAuthPollFloor
	}
	tok, err := rp.DeviceAccessToken(ctx, authResp.DeviceCode, interval, relyingParty)
	if err != nil {
		return fmt.Errorf("device login: %w", err)
	}

	auth := &ServerAuth{
		Issuer:       issuer,
		ClientID:     clientID,
		Scopes:       scopes,
		TokenURL:     relyingParty.OAuthConfig().Endpoint.TokenURL,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
	}
	if tok.ExpiresIn > 0 {
		auth.Expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
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
func ensureFreshToken(ctx context.Context, cc *ClientConfig, name string, ref ServerRef) (string, error) {
	a := ref.Auth
	if a == nil {
		return "", nil
	}
	if a.RefreshToken == "" || a.Expiry.IsZero() || time.Now().Add(expirySkew).Before(a.Expiry) {
		return a.AccessToken, nil
	}

	cfg := &oauth2.Config{
		ClientID: a.ClientID,
		Endpoint: oauth2.Endpoint{TokenURL: a.TokenURL},
		Scopes:   a.Scopes,
	}
	newTok, err := cfg.TokenSource(ctx, &oauth2.Token{
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

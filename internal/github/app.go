// Package github is quack's GitHub App extension (see docs/github-app.md): it
// authenticates as a GitHub App, exposes outbound tools (github_comment,
// github_pull_request) and a git-credential source authed with the App's
// per-installation token, and mounts an inbound, signature-verified webhook
// route that dispatches orchestrator runs on issue-comment mentions.
//
// Auth is done with golang-jwt/jwt (RS256 App JWT) + stdlib net/http for the
// REST calls, NOT go-github/ghinstallation: the flow is one signed JWT plus a
// handful of REST endpoints, and those libraries are a large dependency to save
// ~80 lines in a self-hosted binary (see the docs' "Auth" section).
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// defaultAPIBase is GitHub's REST API root; overridable in tests (httptest).
const defaultAPIBase = "https://api.github.com"

// tokenSkew refreshes an installation token this long before its real expiry,
// so an in-flight git/REST call never races the boundary.
const tokenSkew = 60 * time.Second

// App authenticates as a GitHub App and mints/caches per-installation access
// tokens. Safe for concurrent use.
type App struct {
	appID   int64
	key     *rsa.PrivateKey
	apiBase string
	http    *http.Client

	mu       sync.Mutex
	tokens   map[int64]cachedToken // installation id → token
	installs map[string]int64      // "owner/repo" → installation id (stable; cached forever)
}

type cachedToken struct {
	token   string
	expires time.Time
}

// NewApp builds an App from an app id and a PEM private key (contents, not a
// path). The key is parsed once at startup; a bad key is a clear startup error.
func NewApp(appID int64, pemKey string) (*App, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemKey))
	if err != nil {
		return nil, fmt.Errorf("github: parse private key: %w", err)
	}
	return &App{
		appID:    appID,
		key:      key,
		apiBase:  defaultAPIBase,
		http:     &http.Client{Timeout: 20 * time.Second},
		tokens:   map[int64]cachedToken{},
		installs: map[string]int64{},
	}, nil
}

// LoadPrivateKey returns the PEM key contents from either an inline value or a
// file path (exactly one of which config guarantees is set).
func LoadPrivateKey(inline, path string) (string, error) {
	if inline != "" {
		return inline, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("github: read private_key_path %q: %w", path, err)
	}
	return string(b), nil
}

// appJWT mints a short-lived (≤10 min) RS256 App JWT: iss = app id, iat backdated
// 60s to tolerate clock skew, exp 9 minutes out (under GitHub's 10-minute cap).
func (a *App) appJWT() (string, error) {
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    fmt.Sprintf("%d", a.appID),
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	})
	s, err := tok.SignedString(a.key)
	if err != nil {
		return "", fmt.Errorf("github: sign app jwt: %w", err)
	}
	return s, nil
}

// InstallationToken returns a valid access token for an installation, minting
// (and caching) a fresh one when none is cached or the cached one is within
// tokenSkew of expiry.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	if ct, ok := a.tokens[installationID]; ok && time.Now().Before(ct.expires.Add(-tokenSkew)) {
		a.mu.Unlock()
		return ct.token, nil
	}
	a.mu.Unlock()

	jwtStr, err := a.appJWT()
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := a.doJSON(ctx, http.MethodPost, path, "Bearer "+jwtStr, nil, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("github: installation %d returned an empty token", installationID)
	}
	a.mu.Lock()
	a.tokens[installationID] = cachedToken{token: out.Token, expires: out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

// InstallationForRepo resolves (and caches) the installation id that owns
// owner/repo, via the App JWT. The mapping is stable, so it is cached for the
// process lifetime.
func (a *App) InstallationForRepo(ctx context.Context, owner, repo string) (int64, error) {
	key := owner + "/" + repo
	a.mu.Lock()
	if id, ok := a.installs[key]; ok {
		a.mu.Unlock()
		return id, nil
	}
	a.mu.Unlock()

	jwtStr, err := a.appJWT()
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	path := fmt.Sprintf("/repos/%s/%s/installation", owner, repo)
	if err := a.doJSON(ctx, http.MethodGet, path, "Bearer "+jwtStr, nil, &out); err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("github: no installation found for %s", key)
	}
	a.mu.Lock()
	a.installs[key] = out.ID
	a.mu.Unlock()
	return out.ID, nil
}

// tokenForRepo resolves the installation owning owner/repo and returns a valid
// installation token for it — the one call both the git-credential source and
// the outbound tools go through.
func (a *App) tokenForRepo(ctx context.Context, owner, repo string) (string, error) {
	id, err := a.InstallationForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return a.InstallationToken(ctx, id)
}

// doJSON performs one authenticated REST call: marshals reqBody (nil ⇒ no body),
// sets the Authorization + GitHub headers, and decodes a 2xx JSON response into
// out (nil ⇒ discard). A non-2xx status is an error carrying GitHub's message.
// authz is never logged.
func (a *App) doJSON(ctx context.Context, method, path, authz string, reqBody, out any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("github: marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.apiBase+path, body)
	if err != nil {
		return fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("github: decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// postIssueComment posts a comment on an issue or PR (GitHub treats PR
// conversation comments as issue comments) using the repo's installation token.
func (a *App) postIssueComment(ctx context.Context, owner, repo string, number int, bodyText string) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)
	return a.doJSON(ctx, http.MethodPost, path, "token "+tok, map[string]string{"body": bodyText}, nil)
}

// createPullRequest opens a PR (head → base) using the repo's installation
// token and returns the new PR's html_url.
func (a *App) createPullRequest(ctx context.Context, owner, repo, title, head, base, bodyText string) (string, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	reqBody := map[string]string{"title": title, "head": head, "base": base, "body": bodyText}
	if err := a.doJSON(ctx, http.MethodPost, path, "token "+tok, reqBody, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

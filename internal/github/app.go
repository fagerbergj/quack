// Package github is quack's GitHub App extension (see docs/github-app.md): it
// authenticates as a GitHub App, exposes outbound tools (github_comment,
// github_pull_request, the review-draft tools github_add_review_comment /
// github_list_review_comments / github_delete_review_comment / github_submit_review,
// and the discussion tools github_list_pr_comments / github_reply_to_review_comment /
// github_react_to_comment) and a git-credential source authed with the App's
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
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
	issuer  string // JWT `iss`: the App's Client ID (recommended) or stringified App ID
	key     *rsa.PrivateKey
	apiBase string
	http    *http.Client

	mu        sync.Mutex
	tokens    map[int64]cachedToken // installation id → token
	installs  map[string]int64      // "owner/repo" → installation id (stable; cached forever)
	noInstall map[string]struct{}   // repos the App is NOT installed on (negative cache)
	slug      string                // App's own slug (GET /app), cached; "" until first lookup

	// Review-draft state. A PR review is built up comment-by-comment (each
	// location-validated against the diff at add time) then submitted as one
	// review — see the github_*_review_comment / github_submit_review tools.
	// Ceiling: this state is process-local and ephemeral — a review is drafted
	// and submitted within a single agent run in one process, so a plain
	// in-memory map is enough. ponytail: the GitHub-native "pending review"
	// (create-review-without-event, add-comments, submit-later) would survive a
	// restart / multiple processes — reach for it only if drafts ever need to
	// outlive a run; don't build it now.
	reviewMu sync.Mutex
	drafts   map[string][]reviewComment // "owner/repo#n" → pending inline comments
	diffs    map[string]cachedDiff      // "owner/repo#n" → parsed commentable positions
}

type cachedToken struct {
	token   string
	expires time.Time
}

// diffTTL bounds how long a fetched PR diff's commentable positions are reused,
// so repeated add-comment calls in one review don't re-fetch every time while a
// PR that gains commits mid-review is still picked up reasonably soon.
const diffTTL = 30 * time.Second

// cachedDiff holds a PR's commentable line positions per file, plus when it was
// fetched (for diffTTL expiry).
type cachedDiff struct {
	files   map[string]diffPositions
	fetched time.Time
}

// diffPositions is the set of lines that can carry an inline review comment on
// each side of one file's diff: right = new-file line numbers (added + context),
// left = old-file line numbers (removed + context).
type diffPositions struct {
	right map[int]bool
	left  map[int]bool
}

// NewApp builds an App from a JWT issuer and a PEM private key (contents, not a
// path). The issuer is the App's Client ID (GitHub's recommended `iss`) or its
// stringified App ID — see config.GitHubExtensionConfig.Issuer. GitHub accepts
// either as the App JWT issuer. The key is parsed once at startup; a bad key is
// a clear startup error.
func NewApp(issuer, pemKey string) (*App, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemKey))
	if err != nil {
		return nil, fmt.Errorf("github: parse private key: %w", err)
	}
	return &App{
		issuer:    issuer,
		key:       key,
		apiBase:   defaultAPIBase,
		http:      &http.Client{Timeout: 20 * time.Second},
		tokens:    map[int64]cachedToken{},
		installs:  map[string]int64{},
		noInstall: map[string]struct{}{},
		diffs:     map[string]cachedDiff{},
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

// appJWT mints a short-lived (≤10 min) RS256 App JWT: iss = the App's issuer
// (Client ID or App ID), iat backdated 60s to tolerate clock skew, exp 9 minutes
// out (under GitHub's 10-minute cap).
func (a *App) appJWT() (string, error) {
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    a.issuer,
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
// ErrNoInstallation means the App is not installed on the given repo. It is NOT
// a failure: a PUBLIC repo the App cannot see still clones fine ANONYMOUSLY. Live
// failure this guards against — a code-explorer asked to read OpenHands/goose/
// cloudflare got 404 on every clone because we attached an installation token
// scoped to the operator's own account, breaking repos that need no auth at all.
var ErrNoInstallation = errors.New("github: app has no installation for this repo")

func (a *App) InstallationForRepo(ctx context.Context, owner, repo string) (int64, error) {
	key := owner + "/" + repo
	a.mu.Lock()
	if id, ok := a.installs[key]; ok {
		a.mu.Unlock()
		return id, nil
	}
	if _, miss := a.noInstall[key]; miss {
		a.mu.Unlock()
		return 0, fmt.Errorf("%w: %s", ErrNoInstallation, key)
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
		// 404 = the App simply is not installed on this repo. Cache the miss so we
		// don't re-ask GitHub on every clone, and report it as ErrNoInstallation so
		// callers can fall back to an anonymous (public) clone.
		if strings.Contains(err.Error(), "status 404") {
			a.mu.Lock()
			a.noInstall[key] = struct{}{}
			a.mu.Unlock()
			return 0, fmt.Errorf("%w: %s", ErrNoInstallation, key)
		}
		return 0, err
	}
	if out.ID == 0 {
		a.mu.Lock()
		a.noInstall[key] = struct{}{}
		a.mu.Unlock()
		return 0, fmt.Errorf("%w: %s", ErrNoInstallation, key)
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

// maxGETAttempts bounds the retry loop below: the first try plus up to two
// retries on a transient failure.
const maxGETAttempts = 3

// retryBaseDelay is the first backoff sleep; it doubles each subsequent
// retry (retryBaseDelay, 2*retryBaseDelay, ...).
const retryBaseDelay = 200 * time.Millisecond

// isRetryableStatus reports whether an HTTP status is a transient GitHub
// failure worth retrying: 5xx (server-side, including the "no server
// available" 503 that triggered #467) and 429 (rate limited).
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// doJSON performs one authenticated REST call: marshals reqBody (nil ⇒ no body),
// sets the Authorization + GitHub headers, and decodes a 2xx JSON response into
// out (nil ⇒ discard). A non-2xx status is an error carrying GitHub's message.
// authz is never logged.
//
// GET requests get a bounded retry (up to maxGETAttempts, exponential
// backoff) on a transient failure — a 5xx/429 response or a connection/
// timeout error — since a GET is idempotent and safe to repeat. Every other
// method (POST/PATCH/PUT/DELETE) is tried exactly once: retrying a mutating
// call risks a duplicate (e.g. a comment posted twice).
func (a *App) doJSON(ctx context.Context, method, path, authz string, reqBody, out any) error {
	var bodyBytes []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("github: marshal request: %w", err)
		}
		bodyBytes = b
	}

	attempts := 1
	if method == http.MethodGet {
		attempts = maxGETAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		var body io.Reader
		if bodyBytes != nil {
			body = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, a.apiBase+path, body)
		if err != nil {
			return fmt.Errorf("github: build request: %w", err)
		}
		req.Header.Set("Authorization", authz)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := a.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("github: %s %s: %w", method, path, err)
			if attempt < attempts && retryAfterErr(ctx, attempt) {
				continue
			}
			return lastErr
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("github: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
			if isRetryableStatus(resp.StatusCode) && attempt < attempts && retryAfterErr(ctx, attempt) {
				continue
			}
			return lastErr
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("github: decode %s %s response: %w", method, path, err)
			}
		}
		return nil
	}
	return lastErr
}

// retryAfterErr sleeps an exponential backoff (retryBaseDelay * 2^(attempt-1))
// before a retry, honoring ctx: it returns false (no retry) if ctx is
// cancelled/expires first, so a caller's deadline is never overrun waiting on
// a backoff that will lose anyway.
func retryAfterErr(ctx context.Context, attempt int) bool {
	d := retryBaseDelay * time.Duration(1<<uint(attempt-1))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
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

// editIssueComment overwrites an existing issue/PR comment's body in place —
// the revise-before-post path for a staged comment carrying a delivery marker
// already posted by an earlier run.
func (a *App) editIssueComment(ctx context.Context, owner, repo string, id int64, bodyText string) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, repo, id)
	return a.doJSON(ctx, http.MethodPatch, path, "token "+tok, map[string]string{"body": bodyText}, nil)
}

// createPullRequest opens a PR (head → base) using the repo's installation
// token and returns the new PR's html_url and number.
func (a *App) createPullRequest(ctx context.Context, owner, repo, title, head, base, bodyText string, draft bool) (string, int, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", 0, err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	reqBody := map[string]any{"title": title, "head": head, "base": base, "body": bodyText}
	if draft {
		reqBody["draft"] = true
	}
	if err := a.doJSON(ctx, http.MethodPost, path, "token "+tok, reqBody, &out); err != nil {
		return "", 0, err
	}
	return out.HTMLURL, out.Number, nil
}

// findOpenPR looks up an OPEN pull request already open from head branch, via
// GitHub's own state — not a session's memory of having opened one before —
// so a re-run's revise lands on the SAME PR instead of a duplicate.
// ok is false when none is open (not an error).
func (a *App) findOpenPR(ctx context.Context, owner, repo, branch string) (number int, url string, ok bool, err error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return 0, "", false, err
	}
	var out []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls?head=%s:%s&state=open", owner, repo, owner, branch)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return 0, "", false, err
	}
	if len(out) == 0 {
		return 0, "", false, nil
	}
	return out[0].Number, out[0].HTMLURL, true, nil
}

// updatePullRequest edits an existing PR's title/body — the revise-before-post
// path when findOpenPR finds one already open.
func (a *App) updatePullRequest(ctx context.Context, owner, repo string, number int, title, bodyText string) (string, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	reqBody := map[string]string{"title": title, "body": bodyText}
	if err := a.doJSON(ctx, http.MethodPatch, path, "token "+tok, reqBody, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// branchHeadSHA returns the SHA GitHub reports as branch's current head — the
// ground truth a push must match. A `git push` that exits 0 but the ref never
// actually updates on GitHub's side must not be read as delivered.
func (a *App) branchHeadSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	path := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, branch)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

// listIssueComments fetches an issue's conversation comments (where a posted
// plan lives), flattening the nested user object to a login string.
func (a *App) listIssueComments(ctx context.Context, owner, repo string, number int) ([]commentView, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID        int64     `json:"id"`
		NodeID    string    `json:"node_id"`
		Body      string    `json:"body"`
		User      ghUserRef `json:"user"`
		CreatedAt string    `json:"created_at"`
		UpdatedAt string    `json:"updated_at"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]commentView, 0, len(raw))
	for _, c := range raw {
		out = append(out, commentView{ID: c.ID, NodeID: c.NodeID, Body: c.Body, User: c.User.Login, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	return out, nil
}

// issueMeta fetches an issue or PR's title/body/state/labels via the issues
// endpoint (GitHub treats a PR as an issue for this purpose too, but omits PR
// -only fields like head/base — see pullMeta for those). This is the
// issue-side half of a snapshot fetch (snapshot.go); pullMeta covers PRs.
func (a *App) issueMeta(ctx context.Context, owner, repo string, number int) (title, body, state string, labels []string, err error) {
	tok, terr := a.tokenForRepo(ctx, owner, repo)
	if terr != nil {
		return "", "", "", nil, terr
	}
	var out struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number)
	if err = a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return "", "", "", nil, err
	}
	labels = make([]string, 0, len(out.Labels))
	for _, l := range out.Labels {
		labels = append(labels, l.Name)
	}
	return out.Title, out.Body, out.State, labels, nil
}

// prCommitView is one commit in a PR's commit list (from pulls/{n}/commits) —
// just enough to derive its rebase-stable patch-id (snapshot.go).
type prCommitView struct {
	SHA     string
	Message string
}

// listPRCommits fetches a PR's current commit list (per_page=250 — PRs beyond
// that are rare and the incremental-review delta just sees fewer of them).
func (a *App) listPRCommits(ctx context.Context, owner, repo string, number int) ([]prCommitView, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=250", owner, repo, number)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]prCommitView, 0, len(raw))
	for _, c := range raw {
		out = append(out, prCommitView{SHA: c.SHA, Message: c.Commit.Message})
	}
	return out, nil
}

// commitDiff fetches one commit's unified diff (Accept: vnd.github.v3.diff,
// not doJSON's usual +json — a raw text response, not a JSON envelope). This
// is the input `git patch-id` needs to compute a rebase-stable commit
// identity (snapshot.go) — no local clone required, patch-id reads a diff
// from stdin.
func (a *App) commitDiff(ctx context.Context, owner, repo, sha string) (string, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", a.apiBase, owner, repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("github: build commit diff request: %w", err)
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: commit diff %s: %w", sha, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github: commit diff %s: status %d: %s", sha, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}

// doGraphQL executes one authenticated GraphQL query/mutation against
// apiBase+"/graphql". GitHub's GraphQL errors surface inside a 200 response
// body (not the status code), so they're checked explicitly.
func (a *App) doGraphQL(ctx context.Context, authz, query string, variables map[string]any, out any) error {
	var raw struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	reqBody := map[string]any{"query": query, "variables": variables}
	if err := a.doJSON(ctx, http.MethodPost, "/graphql", authz, reqBody, &raw); err != nil {
		return err
	}
	if len(raw.Errors) > 0 {
		return fmt.Errorf("github: graphql: %s", raw.Errors[0].Message)
	}
	if out != nil && len(raw.Data) > 0 {
		return json.Unmarshal(raw.Data, out)
	}
	return nil
}

// minimizeComment marks a minimizable GitHub node (an issue comment, a PR
// review, a PR review comment — anything with a GraphQL node_id) OUTDATED, so
// the thread shows current state instead of a pile of dead attempts.
// Best-effort: callers log a failure and move on, they never fail delivery.
func (a *App) minimizeComment(ctx context.Context, owner, repo, nodeID string) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	const mutation = `mutation($id: ID!) {
		minimizeComment(input: {subjectId: $id, classifier: OUTDATED}) {
			minimizedComment { isMinimized }
		}
	}`
	return a.doGraphQL(ctx, "token "+tok, mutation, map[string]any{"id": nodeID}, nil)
}

// mergePR squash-merges a pull request using the repo's installation token.
// GitHub's error body (branch protection, conflicts, not mergeable) surfaces
// verbatim via doJSON.
// ponytail: squash only; add a merge_method config when someone wants otherwise.
func (a *App) mergePR(ctx context.Context, owner, repo string, number int) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, number)
	return a.doJSON(ctx, http.MethodPut, path, "token "+tok, map[string]string{"merge_method": "squash"}, nil)
}

// addLabels applies labels to an issue or PR (GitHub's labels endpoint is the
// issues one for both).
func (a *App) addLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, number)
	return a.doJSON(ctx, http.MethodPost, path, "token "+tok, map[string][]string{"labels": labels}, nil)
}

// createReview submits one PR review (a summary body + a verdict event, plus any
// inline path/line comments) using the repo's installation token. It returns the
// review's html_url and id. GitHub 422s if an inline comment's path/line isn't
// part of the PR diff — that message is surfaced verbatim by doJSON.
func (a *App) createReview(ctx context.Context, owner, repo string, number int, event, bodyText string, comments []reviewComment) (string, int64, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", 0, err
	}
	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, number)
	reqBody := map[string]any{"event": event, "body": bodyText}
	if len(comments) > 0 {
		reqBody["comments"] = comments
	}
	if err := a.doJSON(ctx, http.MethodPost, path, "token "+tok, reqBody, &out); err != nil {
		return "", 0, err
	}
	return out.HTMLURL, out.ID, nil
}

// draftKey is the process-local key for one PR's review draft / cached diff.
func draftKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// commentablePositions returns the set of commentable inline positions per file
// for a PR, fetched from GET /pulls/{n}/files and cached for diffTTL so repeated
// add-comment calls in one review don't re-fetch each time.
func (a *App) commentablePositions(ctx context.Context, owner, repo string, number int) (map[string]diffPositions, error) {
	key := draftKey(owner, repo, number)
	a.reviewMu.Lock()
	if cd, ok := a.diffs[key]; ok && time.Since(cd.fetched) < diffTTL {
		a.reviewMu.Unlock()
		return cd.files, nil
	}
	a.reviewMu.Unlock()

	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	// per_page=100 covers all but the largest PRs; a file beyond page 1 simply
	// won't validate (the add tool then reports it as not in the diff) — an
	// acceptable ceiling for a self-hosted reviewer.
	var files []struct {
		Filename string `json:"filename"`
		Patch    string `json:"patch"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", owner, repo, number)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &files); err != nil {
		return nil, err
	}
	positions := make(map[string]diffPositions, len(files))
	for _, f := range files {
		if f.Patch == "" { // binary / too-large files carry no patch
			continue
		}
		positions[f.Filename] = parsePatch(f.Patch)
	}
	a.reviewMu.Lock()
	a.diffs[key] = cachedDiff{files: positions, fetched: time.Now()}
	a.reviewMu.Unlock()
	return positions, nil
}

// parsePatch turns one file's unified-diff patch (from the pulls/files API) into
// the set of commentable line numbers on each side. Right = added ('+') and
// context (' ') lines by new-file number; left = removed ('-') and context lines
// by old-file number.
func parsePatch(patch string) diffPositions {
	pos := diffPositions{right: map[int]bool{}, left: map[int]bool{}}
	var oldLine, newLine int
	for _, ln := range strings.Split(patch, "\n") {
		if strings.HasPrefix(ln, "@@") {
			oldLine, newLine = parseHunkHeader(ln)
			continue
		}
		if ln == "" {
			continue
		}
		switch ln[0] {
		case '+':
			pos.right[newLine] = true
			newLine++
		case '-':
			pos.left[oldLine] = true
			oldLine++
		case '\\': // "\ No newline at end of file"
		default: // context line
			pos.right[newLine] = true
			pos.left[oldLine] = true
			oldLine++
			newLine++
		}
	}
	return pos
}

// parseHunkHeader reads the start lines from a hunk header like
// "@@ -12,7 +15,8 @@ func foo()" → (12, 15). A missing count ("@@ -1 +1 @@") is fine.
func parseHunkHeader(h string) (oldStart, newStart int) {
	for _, f := range strings.Fields(h) {
		switch {
		case strings.HasPrefix(f, "-"):
			oldStart = atoiBeforeComma(f[1:])
		case strings.HasPrefix(f, "+"):
			newStart = atoiBeforeComma(f[1:])
		}
	}
	return
}

func atoiBeforeComma(s string) int {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n
}

// --- Reading & reacting to existing PR discussion ---

// prDiscussion is a PR's existing conversation, so the reviewer sees prior
// context before adding its own: inline review comments, top-level conversation
// comments, and submitted reviews.
type prDiscussion struct {
	ReviewComments []reviewCommentView `json:"review_comments"`
	Comments       []commentView       `json:"comments"`
	Reviews        []reviewView        `json:"reviews"`
}

// reviewCommentView is one inline review comment (from pulls/{n}/comments).
type reviewCommentView struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Body        string `json:"body"`
	User        string `json:"user"`
	InReplyToID int64  `json:"in_reply_to_id,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// commentView is one top-level conversation comment (from issues/{n}/comments).
// NodeID is its GraphQL id, needed only to minimizeComment it; "" for
// call sites (like listPRDiscussion's) that don't fetch it. UpdatedAt is what
// the snapshot diff (snapshot.go) uses to detect an edited comment — GitHub
// bumps it on any body edit, unchanged otherwise.
type commentView struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id,omitempty"`
	Body      string `json:"body"`
	User      string `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// reviewView is one submitted review (from pulls/{n}/reviews).
type reviewView struct {
	ID          int64  `json:"id"`
	Body        string `json:"body"`
	State       string `json:"state"`
	User        string `json:"user"`
	SubmittedAt string `json:"submitted_at"`
}

// listPRDiscussion fetches a PR's inline review comments, conversation comments,
// and submitted reviews (three GETs), flattening GitHub's nested user object to a
// login string.
func (a *App) listPRDiscussion(ctx context.Context, owner, repo string, number int) (prDiscussion, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return prDiscussion{}, err
	}
	authz := "token " + tok
	var out prDiscussion

	var rawReviewComments []struct {
		ID          int64     `json:"id"`
		Path        string    `json:"path"`
		Line        int       `json:"line"`
		Body        string    `json:"body"`
		User        ghUserRef `json:"user"`
		InReplyToID int64     `json:"in_reply_to_id"`
		CreatedAt   string    `json:"created_at"`
	}
	if err := a.doJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100", owner, repo, number), authz, nil, &rawReviewComments); err != nil {
		return prDiscussion{}, err
	}
	for _, c := range rawReviewComments {
		out.ReviewComments = append(out.ReviewComments, reviewCommentView{
			ID: c.ID, Path: c.Path, Line: c.Line, Body: c.Body, User: c.User.Login, InReplyToID: c.InReplyToID, CreatedAt: c.CreatedAt,
		})
	}

	var rawComments []struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		User      ghUserRef `json:"user"`
		CreatedAt string    `json:"created_at"`
	}
	if err := a.doJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number), authz, nil, &rawComments); err != nil {
		return prDiscussion{}, err
	}
	for _, c := range rawComments {
		out.Comments = append(out.Comments, commentView{ID: c.ID, Body: c.Body, User: c.User.Login, CreatedAt: c.CreatedAt})
	}

	var rawReviews []struct {
		ID          int64     `json:"id"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		User        ghUserRef `json:"user"`
		SubmittedAt string    `json:"submitted_at"`
	}
	if err := a.doJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", owner, repo, number), authz, nil, &rawReviews); err != nil {
		return prDiscussion{}, err
	}
	for _, r := range rawReviews {
		out.Reviews = append(out.Reviews, reviewView{ID: r.ID, Body: r.Body, State: r.State, User: r.User.Login, SubmittedAt: r.SubmittedAt})
	}
	return out, nil
}

// ghUserRef decodes GitHub's nested user object down to its login.
type ghUserRef struct {
	Login string `json:"login"`
}

// prReview is one submitted PR review, in the order GitHub returns them
// (chronological). commit_id is GitHub's own durable "reviewed as of" marker —
// the state conversational follow-up reviews key off, so no local store is needed.
// NodeID + Body are only needed to find/collapse quack's own prior reviews.
type prReview struct {
	NodeID      string    `json:"node_id"`
	CommitID    string    `json:"commit_id"`
	State       string    `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, …
	Body        string    `json:"body"`
	User        ghUserRef `json:"user"`
	SubmittedAt string    `json:"submitted_at"`
}

// listReviews fetches a PR's submitted reviews in API (chronological) order.
func (a *App) listReviews(ctx context.Context, owner, repo string, number int) ([]prReview, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var out []prReview
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", owner, repo, number)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// prMeta is a PR's head branch/commit, base branch, title and description. The
// refs let a reviewer check out the changes (git_clone gives a shallow BASE
// clone, so `git diff base...HEAD` is EMPTY until the head is fetched and checked
// out); the title/body give the planner the PR's intent.
type prMeta struct {
	HeadRef string
	HeadSHA string
	BaseRef string
	Title   string
	Body    string
	// State/Labels round out prMeta into the full PR object a snapshot fetch
	// needs (snapshot.go) — the pulls/{n} endpoint already returns both, so
	// snapshot fetch doesn't need issueMeta's separate /issues/{n} call for a PR.
	State  string
	Labels []string
}

// pullMeta fetches a PR's head ref/sha, base ref, title, description,
// state and labels — the git coordinates the reviewer needs plus the intent
// and status the planner/snapshot needs.
func (a *App) pullMeta(ctx context.Context, owner, repo string, number int) (prMeta, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return prMeta{}, err
	}
	var out struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		State string `json:"state"`
		Head  struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return prMeta{}, err
	}
	labels := make([]string, 0, len(out.Labels))
	for _, l := range out.Labels {
		labels = append(labels, l.Name)
	}
	return prMeta{
		HeadRef: out.Head.Ref, HeadSHA: out.Head.SHA, BaseRef: out.Base.Ref,
		Title: out.Title, Body: out.Body, State: out.State, Labels: labels,
	}, nil
}

// changedFile is one file in a PR's diff — path + churn, enough for the planner
// to slice a review by area BEFORE any node clones (the diff itself is not in the
// webhook payload).
type changedFile struct {
	Filename  string `json:"filename"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status"`
}

// pullFiles lists a PR's changed files (per_page=100 — the same large-PR ceiling
// commentablePositions accepts; beyond it the planner just sees fewer slices).
func (a *App) pullFiles(ctx context.Context, owner, repo string, number int) ([]changedFile, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var out []changedFile
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", owner, repo, number)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// botLogin returns quack's own commenting identity ("{app-slug}[bot]"),
// fetched once via GET /app (App-JWT authed, not an installation token) and
// cached for the process lifetime — an App's slug never changes.
func (a *App) botLogin(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.slug != "" {
		s := a.slug
		a.mu.Unlock()
		return s + "[bot]", nil
	}
	a.mu.Unlock()

	jwtStr, err := a.appJWT()
	if err != nil {
		return "", err
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if err := a.doJSON(ctx, http.MethodGet, "/app", "Bearer "+jwtStr, nil, &out); err != nil {
		return "", err
	}
	if out.Slug == "" {
		return "", fmt.Errorf("github: /app returned an empty slug")
	}
	a.mu.Lock()
	a.slug = out.Slug
	a.mu.Unlock()
	return out.Slug + "[bot]", nil
}

// lastReviewedSHA returns the commit_id of quack's most recent review of a PR,
// or "" if quack has never reviewed it (not an error). Prefers a review
// matching quack's own bot login; falls back to the latest review with any
// commit_id if the identity lookup fails or no review matches (e.g. slug
// changed) — still a useful continuation marker.
func (a *App) lastReviewedSHA(ctx context.Context, owner, repo string, number int) (string, error) {
	reviews, err := a.listReviews(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	login, err := a.botLogin(ctx)
	if err != nil {
		login = "" // identity lookup failed; fall back to latest-any below
	}
	var latestAny, latestOwn string
	for _, r := range reviews {
		if r.CommitID == "" {
			continue
		}
		latestAny = r.CommitID
		if login != "" && r.User.Login == login {
			latestOwn = r.CommitID
		}
	}
	if latestOwn != "" {
		return latestOwn, nil
	}
	return latestAny, nil
}

// replyToReviewComment posts an in-thread reply to an existing inline review
// comment via the dedicated replies endpoint, returning the new comment's id and
// html_url.
func (a *App) replyToReviewComment(ctx context.Context, owner, repo string, number int, commentID int64, bodyText string) (int64, string, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return 0, "", err
	}
	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, number, commentID)
	if err := a.doJSON(ctx, http.MethodPost, path, "token "+tok, map[string]string{"body": bodyText}, &out); err != nil {
		return 0, "", err
	}
	return out.ID, out.HTMLURL, nil
}

// reactToComment adds an emoji reaction to a review comment or a conversation
// (issue) comment, returning the reaction id. commentPath selects the endpoint
// family (pulls/comments vs issues/comments).
func (a *App) reactToComment(ctx context.Context, owner, repo, commentPath string, commentID int64, content string) (int64, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	path := fmt.Sprintf("/repos/%s/%s/%s/comments/%d/reactions", owner, repo, commentPath, commentID)
	if err := a.doJSON(ctx, http.MethodPost, path, "token "+tok, map[string]string{"content": content}, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// reactToIssue posts an emoji reaction on the ISSUE itself, returning the
// reaction id. A label event carries no comment ID, so reactToComment can't be
// reused: this targets /repos/{owner}/{repo}/issues/{number}/reactions.
func (a *App) reactToIssue(ctx context.Context, owner, repo string, number int, content string) (int64, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/reactions", owner, repo, number)
	if err := a.doJSON(ctx, http.MethodPost, path, "token "+tok, map[string]string{"content": content}, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

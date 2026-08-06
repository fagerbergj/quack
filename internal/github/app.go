// Package github is quack's GitHub App extension: auth, tools, webhook dispatch.
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

const defaultAPIBase = "https://api.github.com"
const tokenSkew = 60 * time.Second

// App authenticates as a GitHub App and caches per-installation tokens.
type App struct {
	issuer          string
	key             *rsa.PrivateKey
	apiBase         string
	http            *http.Client
	partialFixLabel string

	mu        sync.Mutex
	tokens    map[int64]cachedToken
	installs  map[string]int64
	noInstall map[string]struct{}
	slug      string

	reviewMu sync.Mutex
	drafts   map[string][]reviewComment
	diffs    map[string]cachedDiff
	// ponytail: use GitHub's native "pending review" if drafts ever need to outlive a single run.
}

type cachedToken struct {
	token   string
	expires time.Time
}

const diffTTL = 30 * time.Second

type cachedDiff struct {
	files   map[string]diffPositions
	fetched time.Time
}

type diffPositions struct {
	right map[int]bool
	left  map[int]bool
}

func NewApp(issuer, pemKey string) (*App, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemKey))
	if err != nil {
		return nil, fmt.Errorf("github: parse private key: %w", err)
	}
	return &App{
		issuer:          issuer,
		key:             key,
		apiBase:         defaultAPIBase,
		http:            &http.Client{Timeout: 20 * time.Second},
		tokens:          map[int64]cachedToken{},
		installs:        map[string]int64{},
		noInstall:       map[string]struct{}{},
		diffs:           map[string]cachedDiff{},
		partialFixLabel: defaultPartialFixLabel,
	}, nil
}

const defaultPartialFixLabel = "quack:partial-fix"

func (a *App) SetPartialFixLabel(label string) {
	if label != "" {
		a.partialFixLabel = label
	}
}

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

// appJWT mints a short-lived (≤10 min) RS256 App JWT.
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

func (a *App) tokenForRepo(ctx context.Context, owner, repo string) (string, error) {
	id, err := a.InstallationForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return a.InstallationToken(ctx, id)
}

const maxGETAttempts = 3
const retryBaseDelay = 200 * time.Millisecond

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

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

func (a *App) postIssueComment(ctx context.Context, owner, repo string, number int, bodyText string) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)
	return a.doJSON(ctx, http.MethodPost, path, "token "+tok, map[string]string{"body": bodyText}, nil)
}

func (a *App) editIssueComment(ctx context.Context, owner, repo string, id int64, bodyText string) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, repo, id)
	return a.doJSON(ctx, http.MethodPatch, path, "token "+tok, map[string]string{"body": bodyText}, nil)
}

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

// updatePullRequest PATCHes only the fields the caller actually supplied - an
// omitted key leaves that field untouched on GitHub rather than blanking it
// (#724: a push with nothing to say must not erase the PR's real title/body).
func (a *App) updatePullRequest(ctx context.Context, owner, repo string, number int, title string, titleSet bool, bodyText string, bodySet bool) (string, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	reqBody := map[string]string{}
	if titleSet {
		reqBody["title"] = title
	}
	if bodySet {
		reqBody["body"] = bodyText
	}
	if err := a.doJSON(ctx, http.MethodPatch, path, "token "+tok, reqBody, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

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

const pushVerifyAttempts = 4
const pushVerifyBaseDelay = 300 * time.Millisecond

func (a *App) verifyPushedBranch(ctx context.Context, owner, repo, branch string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= pushVerifyAttempts; attempt++ {
		sha, err := a.branchHeadSHA(ctx, owner, repo, branch)
		if err == nil {
			return sha, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "status 404") {
			return "", err
		}
		if attempt == pushVerifyAttempts {
			break
		}
		delay := pushVerifyBaseDelay * time.Duration(1<<uint(attempt-1))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		}
	}
	return "", lastErr
}

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

func (a *App) issueMeta(ctx context.Context, owner, repo string, number int) (title, body, state string, labels []string, isPR bool, err error) {
	tok, terr := a.tokenForRepo(ctx, owner, repo)
	if terr != nil {
		return "", "", "", nil, false, terr
	}
	var out struct {
		Title       string    `json:"title"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		PullRequest *struct{} `json:"pull_request"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number)
	if err = a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return "", "", "", nil, false, err
	}
	labels = make([]string, 0, len(out.Labels))
	for _, l := range out.Labels {
		labels = append(labels, l.Name)
	}
	return out.Title, out.Body, out.State, labels, out.PullRequest != nil, nil
}

type prCommitView struct {
	SHA     string
	Message string
}

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

// commitDiff fetches one commit's unified diff (Accept: vnd.github.v3.diff) for git patch-id.
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

// mergePR squash-merges a PR. headSHA pins to a specific commit;
// ponytail: squash only; add merge_method config when someone wants otherwise.
func (a *App) mergePR(ctx context.Context, owner, repo string, number int, requiredHeadSHA string) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	body := map[string]string{"merge_method": "squash"}
	if requiredHeadSHA != "" {
		body["sha"] = requiredHeadSHA
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, number)
	return a.doJSON(ctx, http.MethodPut, path, "token "+tok, body, nil)
}

type checkRunView struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

func (a *App) listCheckRuns(ctx context.Context, owner, repo, sha string) ([]checkRunView, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var out struct {
		CheckRuns []checkRunView `json:"check_runs"`
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", owner, repo, sha)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return nil, err
	}
	return out.CheckRuns, nil
}

type checkAnnotation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	Level     string `json:"annotation_level"`
	Message   string `json:"message"`
}

func (a *App) listCheckAnnotations(ctx context.Context, owner, repo string, checkRunID int64) ([]checkAnnotation, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var out []checkAnnotation
	path := fmt.Sprintf("/repos/%s/%s/check-runs/%d/annotations?per_page=50", owner, repo, checkRunID)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *App) addLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, number)
	return a.doJSON(ctx, http.MethodPost, path, "token "+tok, map[string][]string{"labels": labels}, nil)
}

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

func draftKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

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
		if f.Patch == "" {
			continue
		}
		positions[f.Filename] = parsePatch(f.Patch)
	}
	a.reviewMu.Lock()
	a.diffs[key] = cachedDiff{files: positions, fetched: time.Now()}
	a.reviewMu.Unlock()
	return positions, nil
}

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
		case '\\':
		default:
			pos.right[newLine] = true
			pos.left[oldLine] = true
			oldLine++
			newLine++
		}
	}
	return pos
}

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

type prDiscussion struct {
	ReviewComments []reviewCommentView `json:"review_comments"`
	Comments       []commentView       `json:"comments"`
	Reviews        []reviewView        `json:"reviews"`
}

type reviewCommentView struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Body        string `json:"body"`
	User        string `json:"user"`
	InReplyToID int64  `json:"in_reply_to_id,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type commentView struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id,omitempty"`
	Body      string `json:"body"`
	User      string `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type reviewView struct {
	ID          int64  `json:"id"`
	Body        string `json:"body"`
	State       string `json:"state"`
	User        string `json:"user"`
	SubmittedAt string `json:"submitted_at"`
}

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

type ghUserRef struct {
	Login string `json:"login"`
}

type prReview struct {
	NodeID      string    `json:"node_id"`
	CommitID    string    `json:"commit_id"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	User        ghUserRef `json:"user"`
	SubmittedAt string    `json:"submitted_at"`
}

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

type prMeta struct {
	HeadRef string
	HeadSHA string
	BaseRef string
	Title   string
	Body    string
	State   string
	Labels  []string
	Fork    bool // head repo differs from base — cannot push to fork branches (#662)
}

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
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
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
		Fork: out.Head.Repo.FullName != out.Base.Repo.FullName,
	}, nil
}

type changedFile struct {
	Filename  string `json:"filename"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status"`
}

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

func (a *App) prAuthor(ctx context.Context, owner, repo string, number int) (string, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	var out struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return "", err
	}
	return out.User.Login, nil
}

func (a *App) commitAuthorEmail(ctx context.Context, owner, repo, sha string) (string, error) {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	var out struct {
		Commit struct {
			Author struct {
				Email string `json:"email"`
			} `json:"author"`
		} `json:"commit"`
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s", owner, repo, sha)
	if err := a.doJSON(ctx, http.MethodGet, path, "token "+tok, nil, &out); err != nil {
		return "", err
	}
	return out.Commit.Author.Email, nil
}

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

func (a *App) lastReviewedSHA(ctx context.Context, owner, repo string, number int) (string, error) {
	reviews, err := a.listReviews(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	login, err := a.botLogin(ctx)
	if err != nil {
		login = ""
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

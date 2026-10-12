package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fagerbergj/quack-extensions/github/internal/httpx"
	"github.com/fagerbergj/quack-extensions/sdk"
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
	version         string // quack build stamp; "" = omit from posted footers
	publicURL       string // externally reachable base URL; "" = footers carry no run link
	mention         string
	labels          Labels
	triggers        map[string]bool // enabled triggers; nil until SetReviewCommands runs

	mu        sync.Mutex
	tokens    map[int64]cachedToken
	installs  map[string]int64
	noInstall map[string]struct{}
	slug      string

	reviewMu sync.Mutex
	diffs    map[string]cachedDiff

	decide func(context.Context, sdk.DecideRequest) (sdk.Decision, error) // nil = no decision handler
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
	patch string
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
		http:            &http.Client{Timeout: 20 * time.Second, Transport: httpx.NewTransport(nil, 0, 0)},
		tokens:          map[int64]cachedToken{},
		installs:        map[string]int64{},
		noInstall:       map[string]struct{}{},
		diffs:           map[string]cachedDiff{},
		partialFixLabel: defaultPartialFixLabel,
	}, nil
}

func (a *App) SetPartialFixLabel(label string) {
	if label != "" {
		a.partialFixLabel = label
	}
}

// SetAPIBase points the client at something other than api.github.com, e.g. the QA mock server.
func (a *App) SetAPIBase(base string) {
	if base != "" {
		a.apiBase = strings.TrimRight(base, "/")
	}
}

// SetFooter records the build stamp and public base URL withFooter stamps on posted bodies;
// either may be "".
func (a *App) SetFooter(version, publicURL string) {
	a.version = version
	a.publicURL = strings.TrimRight(publicURL, "/")
}

// SetReviewCommands records the deployment's mention token, label names and enabled triggers,
// which a posted review's commands block is built from.
func (a *App) SetReviewCommands(mention string, labels Labels, triggers map[string]bool) {
	a.mention = mention
	a.labels = labels
	a.triggers = triggers
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

// doJSON issues one request; retry (GET only, on 429/5xx/connection faults)
// happens transparently inside a.http's transport - see internal/httpx.
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
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

// repoJSON is doJSON authorized with owner/repo's installation token.
func (a *App) repoJSON(ctx context.Context, owner, repo, method, path string, reqBody, out any) error {
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return a.doJSON(ctx, method, path, "token "+tok, reqBody, out)
}

// setCommentBody writes bodyText (mentions stripped) as {"body": ...}.
func (a *App) setCommentBody(ctx context.Context, owner, repo, method, path, bodyText string) error {
	return a.repoJSON(ctx, owner, repo, method, path, map[string]string{"body": stripMentions(bodyText)}, nil)
}

func (a *App) postIssueComment(ctx context.Context, owner, repo string, number int, bodyText string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)
	return a.setCommentBody(ctx, owner, repo, http.MethodPost, path, bodyText)
}

func (a *App) editIssueComment(ctx context.Context, owner, repo string, id int64, bodyText string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, repo, id)
	return a.setCommentBody(ctx, owner, repo, http.MethodPatch, path, bodyText)
}

// updateReview replaces the body of one of quack's own reviews, so a merge outcome lands on
// the approving review instead of a new comment.
func (a *App) updateReview(ctx context.Context, owner, repo string, number int, reviewID int64, bodyText string) error {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d", owner, repo, number, reviewID)
	return a.setCommentBody(ctx, owner, repo, http.MethodPut, path, bodyText)
}

func (a *App) createPullRequest(ctx context.Context, owner, repo, title, head, base, bodyText string, draft bool) (string, int, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls", owner, repo)
	reqBody := map[string]any{"title": stripMentions(title), "head": head, "base": base, "body": stripMentions(bodyText)}
	if draft {
		reqBody["draft"] = true
	}
	if err := a.repoJSON(ctx, owner, repo, http.MethodPost, path, reqBody, &out); err != nil {
		return "", 0, err
	}
	return out.HTMLURL, out.Number, nil
}

func (a *App) findOpenPR(ctx context.Context, owner, repo, branch string) (number int, url string, ok bool, err error) {
	var out []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls?head=%s:%s&state=open", owner, repo, owner, branch)
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &out); err != nil {
		return 0, "", false, err
	}
	if len(out) == 0 {
		return 0, "", false, nil
	}
	return out[0].Number, out[0].HTMLURL, true, nil
}

// updatePullRequest PATCHes only the supplied fields: an omitted key leaves GitHub's title/body
// untouched rather than blanking it.
func (a *App) updatePullRequest(ctx context.Context, owner, repo string, number int, title string, titleSet bool, bodyText string, bodySet bool) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number)
	reqBody := map[string]string{}
	if titleSet {
		reqBody["title"] = stripMentions(title)
	}
	if bodySet {
		reqBody["body"] = stripMentions(bodyText)
	}
	if err := a.repoJSON(ctx, owner, repo, http.MethodPatch, path, reqBody, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

func (a *App) branchHeadSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	path := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, branch)
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &out); err != nil {
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
	var raw []struct {
		ID        int64     `json:"id"`
		NodeID    string    `json:"node_id"`
		Body      string    `json:"body"`
		User      ghUserRef `json:"user"`
		CreatedAt string    `json:"created_at"`
		UpdatedAt string    `json:"updated_at"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number)
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]commentView, 0, len(raw))
	for _, c := range raw {
		out = append(out, commentView{ID: c.ID, NodeID: c.NodeID, Body: c.Body, User: c.User.Login, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	return out, nil
}

func (a *App) issueMeta(ctx context.Context, owner, repo string, number int) (title, body, state string, labels []string, isPR bool, err error) {
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
	if err = a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &out); err != nil {
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
	var raw []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=250", owner, repo, number)
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &raw); err != nil {
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
	defer func() { _ = resp.Body.Close() }()
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

// mergePR squash-merges a PR pinned to requiredHeadSHA and returns the merge commit sha.
// ponytail: squash only; add merge_method config when someone wants otherwise.
func (a *App) mergePR(ctx context.Context, owner, repo string, number int, requiredHeadSHA string) (string, error) {
	body := map[string]string{"merge_method": "squash"}
	if requiredHeadSHA != "" {
		body["sha"] = requiredHeadSHA
	}
	var out struct {
		SHA string `json:"sha"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, number)
	if err := a.repoJSON(ctx, owner, repo, http.MethodPut, path, body, &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

type checkRunView struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"` // "queued" | "in_progress" | "completed"
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
	// Why is bounded failure detail filled by enrichFailingChecks, only for failure/timed_out runs.
	Why []string `json:"-"`
}

// enrichFailingChecks fills Why on up to 3 failing runs (2 annotation lines each, output title
// as fallback) so the envelope can say what broke.
func (a *App) enrichFailingChecks(ctx context.Context, owner, repo string, runs []checkRunView) {
	enriched := 0
	for i := range runs {
		r := &runs[i]
		if r.Conclusion != "failure" && r.Conclusion != "timed_out" {
			continue
		}
		if enriched >= 3 {
			return
		}
		enriched++
		anns, err := a.listCheckAnnotations(ctx, owner, repo, r.ID)
		if err == nil {
			for _, an := range anns {
				if an.Level != "failure" && an.Level != "warning" {
					continue
				}
				r.Why = append(r.Why, truncate(fmt.Sprintf("%s:%d %s", an.Path, an.StartLine, an.Message), 200))
				if len(r.Why) >= 2 {
					break
				}
			}
		}
		if t := strings.TrimSpace(r.Output.Title); len(r.Why) == 0 && t != "" {
			r.Why = []string{truncate(t, 200)}
		}
	}
}

func (a *App) listCheckRuns(ctx context.Context, owner, repo, sha string) ([]checkRunView, error) {
	var out = &struct {
		CheckRuns []checkRunView `json:"check_runs"`
	}{}
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", owner, repo, sha), nil, out); err != nil {
		return nil, err
	}
	return out.CheckRuns, nil
}

// checkSuiteView is one check-suite: created for a head at push time, before
// any check run exists, for every workflow the push triggers.
type checkSuiteView struct {
	ID                   int64  `json:"id"`
	Status               string `json:"status"`
	Conclusion           string `json:"conclusion"`
	LatestCheckRunsCount int    `json:"latest_check_runs_count"`
	App                  struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

// producesRuns: GitHub Actions suites always post runs; other apps count only once they have,
// since some register purely informational suites that never populate one.
func (s checkSuiteView) producesRuns() bool {
	return s.App.Slug == "github-actions" || s.LatestCheckRunsCount > 0
}

func (a *App) listCheckSuites(ctx context.Context, owner, repo, sha string) ([]checkSuiteView, error) {
	type suitesPage struct {
		CheckSuites []checkSuiteView `json:"check_suites"`
	}
	var out suitesPage
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s/check-suites?per_page=100", owner, repo, sha), nil, &out); err != nil {
		return nil, err
	}
	return out.CheckSuites, nil
}

type checkAnnotation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	Level     string `json:"annotation_level"`
	Message   string `json:"message"`
}

func (a *App) listCheckAnnotations(ctx context.Context, owner, repo string, checkRunID int64) ([]checkAnnotation, error) {
	var out []checkAnnotation
	path := fmt.Sprintf("/repos/%s/%s/check-runs/%d/annotations?per_page=50", owner, repo, checkRunID)
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *App) addLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, number)
	return a.repoJSON(ctx, owner, repo, http.MethodPost, path, map[string][]string{"labels": labels}, nil)
}

func (a *App) createReview(ctx context.Context, owner, repo string, number int, event, bodyText string, comments []reviewComment) (string, int64, error) {
	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, number)
	reqBody := map[string]any{"event": event, "body": stripMentions(bodyText)}
	if len(comments) > 0 {
		clean := make([]reviewComment, len(comments))
		for i, c := range comments {
			c.Body = stripMentions(c.Body)
			clean[i] = c
		}
		reqBody["comments"] = clean
	}
	if err := a.repoJSON(ctx, owner, repo, http.MethodPost, path, reqBody, &out); err != nil {
		return "", 0, err
	}
	return out.HTMLURL, out.ID, nil
}

func (a *App) commentablePositions(ctx context.Context, owner, repo string, number int) (map[string]diffPositions, error) {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	a.reviewMu.Lock()
	if cd, ok := a.diffs[key]; ok && time.Since(cd.fetched) < diffTTL {
		a.reviewMu.Unlock()
		return cd.files, nil
	}
	a.reviewMu.Unlock()

	var files []struct {
		Filename string `json:"filename"`
		Patch    string `json:"patch"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", owner, repo, number)
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &files); err != nil {
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
	maps.DeleteFunc(a.diffs, func(_ string, cd cachedDiff) bool { return time.Since(cd.fetched) >= diffTTL })
	a.diffs[key] = cachedDiff{files: positions, fetched: time.Now()}
	a.reviewMu.Unlock()
	return positions, nil
}

func parsePatch(patch string) diffPositions {
	pos := diffPositions{right: map[int]bool{}, left: map[int]bool{}, patch: patch}
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
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100", owner, repo, number), nil, &rawReviewComments); err != nil {
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
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number), nil, &rawComments); err != nil {
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
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", owner, repo, number), nil, &rawReviews); err != nil {
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
	ID          int64     `json:"id"`
	HTMLURL     string    `json:"html_url"`
	NodeID      string    `json:"node_id"`
	CommitID    string    `json:"commit_id"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	User        ghUserRef `json:"user"`
	SubmittedAt string    `json:"submitted_at"`
}

func (a *App) listReviews(ctx context.Context, owner, repo string, number int) ([]prReview, error) {
	var out []prReview
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", owner, repo, number), nil, &out); err != nil {
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
	Draft   bool
	Merged  bool
	Labels  []string
	Fork    bool // head repo differs from base: cannot push to fork branches
}

func (a *App) pullMeta(ctx context.Context, owner, repo string, number int) (prMeta, error) {
	var out struct {
		Title    string     `json:"title"`
		Body     string     `json:"body"`
		State    string     `json:"state"`
		Draft    bool       `json:"draft"`
		MergedAt *time.Time `json:"merged_at"`
		Head     struct {
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
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &out); err != nil {
		return prMeta{}, err
	}
	labels := make([]string, 0, len(out.Labels))
	for _, l := range out.Labels {
		labels = append(labels, l.Name)
	}
	return prMeta{
		HeadRef: out.Head.Ref, HeadSHA: out.Head.SHA, BaseRef: out.Base.Ref,
		Title: out.Title, Body: out.Body, State: out.State, Draft: out.Draft,
		Merged: out.MergedAt != nil, Labels: labels,
		Fork: out.Head.Repo.FullName != out.Base.Repo.FullName,
	}, nil
}

// mergeLabelActor finds who last applied or removed label via the timeline, the only place
// GitHub records that actor. known is false when the label never appears; callers fail closed.
func (a *App) mergeLabelActor(ctx context.Context, owner, repo string, number int, label string) (actor string, stillApplied, known bool, err error) {
	type timelineEvent struct {
		Event     string `json:"event"`
		CreatedAt string `json:"created_at"`
		Actor     struct {
			Login string `json:"login"`
		} `json:"actor"`
		Label struct {
			Name string `json:"name"`
		} `json:"label"`
	}
	var latest string
	for page := 1; ; page++ {
		var events []timelineEvent
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?per_page=100&page=%d", owner, repo, number, page)
		if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &events); err != nil {
			return "", false, false, err
		}
		// Compare created_at, not list order, so page ordering can't matter.
		for _, e := range events {
			if e.Label.Name == label && (e.Event == "labeled" || e.Event == "unlabeled") && e.CreatedAt >= latest {
				latest, actor, stillApplied, known = e.CreatedAt, e.Actor.Login, e.Event == "labeled", true
			}
		}
		if len(events) < 100 {
			return actor, stillApplied, known, nil
		}
	}
}

type changedFile struct {
	Filename  string `json:"filename"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status"`
}

func (a *App) pullFiles(ctx context.Context, owner, repo string, number int) ([]changedFile, error) {
	var out []changedFile
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", owner, repo, number), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *App) prAuthor(ctx context.Context, owner, repo string, number int) (string, error) {
	type authorPage struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	var out authorPage
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number), nil, &out); err != nil {
		return "", err
	}
	return out.User.Login, nil
}

func (a *App) commitAuthorEmail(ctx context.Context, owner, repo, sha string) (string, error) {
	var out struct {
		Commit struct {
			Author struct {
				Email string `json:"email"`
			} `json:"author"`
		} `json:"commit"`
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s", owner, repo, sha)
	if err := a.repoJSON(ctx, owner, repo, http.MethodGet, path, nil, &out); err != nil {
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

func (a *App) replyToReviewComment(ctx context.Context, owner, repo string, number int, commentID int64, bodyText string) (int64, string, error) {
	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, number, commentID)
	if err := a.repoJSON(ctx, owner, repo, http.MethodPost, path, map[string]string{"body": stripMentions(bodyText)}, &out); err != nil {
		return 0, "", err
	}
	return out.ID, out.HTMLURL, nil
}

func (a *App) reactToComment(ctx context.Context, owner, repo, commentPath string, commentID int64, content string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	path := fmt.Sprintf("/repos/%s/%s/%s/comments/%d/reactions", owner, repo, commentPath, commentID)
	if err := a.repoJSON(ctx, owner, repo, http.MethodPost, path, map[string]string{"content": content}, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

func (a *App) reactToIssue(ctx context.Context, owner, repo string, number int, content string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/reactions", owner, repo, number)
	if err := a.repoJSON(ctx, owner, repo, http.MethodPost, path, map[string]string{"content": content}, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

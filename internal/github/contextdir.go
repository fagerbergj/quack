// Sibling context directory (#660, docs/trigger-prompts-v2.md "Context
// directory"): every `truncate()` in the trigger path destroys information an
// agent may need, so WriteContextDir dumps the GitHub API responses whole,
// one file per endpoint, into a directory sibling to the provisioned clone
// (workspace.ContextDirScope - never inside the clone, nothing to gitignore).
// Nothing consumes these files yet; this is a pure addition.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// GitHub's own hard ceilings on these two endpoints (not ours) - see
// commentablePositions' doc above and GitHub's PR-commits docs. WriteContextDir
// states these explicitly in the file when hit (a response that looks whole
// and isn't is worse than an obviously partial one), rather than silently
// returning a short list.
const (
	maxPRFiles   = 3000
	maxPRCommits = 250
)

// ContextRequest identifies the GitHub thread WriteContextDir dumps and which
// conditional files apply.
type ContextRequest struct {
	Owner, Repo string
	Number      int
	IsPR        bool
	// CheckSHA is the commit whose check runs to dump - set only when this
	// run was dispatched by a CI event. "" skips check-runs.json and every
	// annotations-*.json: a plan/review/mention run has no single check run
	// it's reacting to.
	CheckSHA string
}

// WriteContextDir fetches every endpoint the trigger envelope needs and writes
// each as its own file in dir, named by endpoint - the full response,
// unfiltered (filtering happens in the envelope). dir must already exist;
// this never creates or resolves it.
//
// Fails soft per file: a file is written ONLY on success, since a missing
// file reads as "not fetched" but an empty one would lie as "no data". The
// only returned error is dir itself being unusable.
func (a *App) WriteContextDir(ctx context.Context, dir string, req ContextRequest) error {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("github: context dir %q is not a directory: %w", dir, err)
	}
	tok, err := a.tokenForRepo(ctx, req.Owner, req.Repo)
	if err != nil {
		slog.Warn("github: context dir: could not authenticate; nothing written", "component", "github",
			"repo", req.Owner+"/"+req.Repo, "number", req.Number, "err", err)
		return nil
	}
	authz := "token " + tok

	a.writeObject(ctx, dir, "issue.json", authz, fmt.Sprintf("/repos/%s/%s/issues/%d", req.Owner, req.Repo, req.Number))
	a.writeList(ctx, dir, "comments.json", authz,
		fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", req.Owner, req.Repo, req.Number), 0, "")

	if !req.IsPR {
		a.writeList(ctx, dir, "timeline.json", authz,
			fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?per_page=100", req.Owner, req.Repo, req.Number), 0, "")
		return nil
	}

	pull := a.writeObject(ctx, dir, "pull.json", authz, fmt.Sprintf("/repos/%s/%s/pulls/%d", req.Owner, req.Repo, req.Number))
	a.writeList(ctx, dir, "files.json", authz,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", req.Owner, req.Repo, req.Number),
		maxPRFiles, fmt.Sprintf("GitHub caps GET /pulls/%d/files at %d files; this PR's file list was cut off there.", req.Number, maxPRFiles))
	a.writeList(ctx, dir, "commits.json", authz,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100", req.Owner, req.Repo, req.Number),
		maxPRCommits, fmt.Sprintf("GitHub caps GET /pulls/%d/commits at %d commits; this PR's commit list was cut off there.", req.Number, maxPRCommits))
	a.writeList(ctx, dir, "reviews.json", authz,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", req.Owner, req.Repo, req.Number), 0, "")
	a.writeList(ctx, dir, "review-comments.json", authz,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100", req.Owner, req.Repo, req.Number), 0, "")

	if req.CheckSHA != "" {
		for _, r := range a.writeCheckRuns(ctx, dir, authz, req.Owner, req.Repo, req.CheckSHA) {
			if !r.failed {
				continue
			}
			a.writeList(ctx, dir, "annotations-"+sanitizeCheckName(r.name)+".json", authz,
				fmt.Sprintf("/repos/%s/%s/check-runs/%d/annotations?per_page=100", req.Owner, req.Repo, r.id), 0, "")
		}
	}

	if pull != nil {
		for _, n := range linkedIssueNumbers(pull) {
			a.writeObject(ctx, dir, fmt.Sprintf("linked-issue-%d.json", n), authz,
				fmt.Sprintf("/repos/%s/%s/issues/%d", req.Owner, req.Repo, n))
			a.writeList(ctx, dir, fmt.Sprintf("linked-issue-%d-comments.json", n), authz,
				fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", req.Owner, req.Repo, n), 0, "")
		}
	}
	return nil
}

// writeObject fetches ONE GitHub object endpoint and writes it verbatim as
// dir/name - nil (and a WARN, never an error) on any fetch or write failure,
// per WriteContextDir's fail-soft contract.
func (a *App) writeObject(ctx context.Context, dir, name, authz, path string) json.RawMessage {
	var raw json.RawMessage
	if err := a.doJSON(ctx, http.MethodGet, path, authz, nil, &raw); err != nil {
		slog.Warn("github: context dir: fetch failed; skipping", "component", "github", "file", name, "err", err)
		return nil
	}
	if err := writeIndentedRaw(filepath.Join(dir, name), raw); err != nil {
		slog.Warn("github: context dir: write failed; skipping", "component", "github", "file", name, "err", err)
		return nil
	}
	return raw
}

// writeList fetches a GitHub list endpoint to exhaustion (fetchAllPages) and
// writes it as dir/name - a bare JSON array normally, or, when cap>0 and
// GitHub's own ceiling was actually hit, wrapped {"items":…,"truncated":true,
// "note":capNote} so a response that LOOKS whole and isn't says so. Same
// fail-soft contract as writeObject; returns the fetched items (nil on any
// failure) so a caller can inspect them (linkedIssueNumbers, check-run ids)
// without a second fetch.
func (a *App) writeList(ctx context.Context, dir, name, authz, firstPath string, cap int, capNote string) []json.RawMessage {
	items, truncated, err := a.fetchAllPages(ctx, firstPath, authz, cap)
	if err != nil {
		slog.Warn("github: context dir: fetch failed; skipping", "component", "github", "file", name, "err", err)
		return nil
	}
	if items == nil {
		items = []json.RawMessage{} // a genuinely empty result is "[]", GitHub's own shape - never "null"
	}
	var payload any = items
	if truncated {
		payload = map[string]any{"items": items, "truncated": true, "note": capNote}
	}
	if err := writeIndentedValue(filepath.Join(dir, name), payload); err != nil {
		slog.Warn("github: context dir: write failed; skipping", "component", "github", "file", name, "err", err)
		return nil
	}
	return items
}

// checkRunSummary is the slice of one check run WriteContextDir needs to
// decide whether to fetch its annotations - the full run is already in
// check-runs.json.
type checkRunSummary struct {
	id     int64
	name   string
	failed bool
}

// writeCheckRuns fetches and writes check-runs.json - unlike every other
// list endpoint here, GitHub wraps this one's array in {"total_count",
// "check_runs"} rather than returning it bare, so it can't reuse writeList.
func (a *App) writeCheckRuns(ctx context.Context, dir, authz, owner, repo, sha string) []checkRunSummary {
	items, err := a.fetchCheckRuns(ctx, authz, owner, repo, sha)
	if err != nil {
		slog.Warn("github: context dir: fetch failed; skipping", "component", "github", "file", "check-runs.json", "err", err)
		return nil
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	payload := map[string]any{"total_count": len(items), "check_runs": items}
	if err := writeIndentedValue(filepath.Join(dir, "check-runs.json"), payload); err != nil {
		slog.Warn("github: context dir: write failed; skipping", "component", "github", "file", "check-runs.json", "err", err)
		return nil
	}
	out := make([]checkRunSummary, 0, len(items))
	for _, raw := range items {
		var r struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		// Same failing-conclusion set cifix.go's failingChecks uses.
		failed := r.Conclusion == "failure" || r.Conclusion == "timed_out"
		out = append(out, checkRunSummary{id: r.ID, name: r.Name, failed: failed})
	}
	return out
}

// fetchCheckRuns pages through GET /commits/{sha}/check-runs, concatenating
// the "check_runs" array across pages (see writeCheckRuns' doc on the
// wrapped-object shape).
func (a *App) fetchCheckRuns(ctx context.Context, authz, owner, repo, sha string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	next := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", owner, repo, sha)
	for next != "" {
		var page struct {
			CheckRuns []json.RawMessage `json:"check_runs"`
		}
		nextURL, err := a.doPagedGET(ctx, next, authz, &page)
		if err != nil {
			return all, err
		}
		all = append(all, page.CheckRuns...)
		next = nextURL
	}
	return all, nil
}

// fetchAllPages follows a GitHub list endpoint's Link header to exhaustion -
// a partial page is the failure mode this whole file exists to kill. cap,
// when > 0, is one of GitHub's OWN documented ceilings (see WriteContextDir's
// maxPRFiles/maxPRCommits callers); reaching it stops early and reports
// truncated, rather than silently returning a response that looks complete.
func (a *App) fetchAllPages(ctx context.Context, firstPath, authz string, cap int) (items []json.RawMessage, truncated bool, err error) {
	next := firstPath
	for next != "" {
		var page []json.RawMessage
		nextURL, perr := a.doPagedGET(ctx, next, authz, &page)
		if perr != nil {
			return items, false, perr
		}
		items = append(items, page...)
		if cap > 0 && len(items) >= cap {
			return items[:cap], true, nil
		}
		next = nextURL
	}
	return items, false, nil
}

// doPagedGET performs one authenticated GET, decoding the response into out
// (a bare-array target like *[]json.RawMessage, or a wrapper struct for an
// endpoint like check-runs that nests its array) and returning the Link
// header's "next" page URL ("" past the last page). Mirrors doJSON's GET
// retry policy (5xx/429, exponential backoff) - duplicated rather than
// refactored into doJSON, which has no caller that needs response headers.
func (a *App) doPagedGET(ctx context.Context, pathOrURL, authz string, out any) (string, error) {
	url := pathOrURL
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = a.apiBase + pathOrURL
	}
	var lastErr error
	for attempt := 1; attempt <= maxGETAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", fmt.Errorf("github: build request: %w", err)
		}
		req.Header.Set("Authorization", authz)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := a.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("github: GET %s: %w", url, err)
			if attempt < maxGETAttempts && retryAfterErr(ctx, attempt) {
				continue
			}
			return "", lastErr
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("github: GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(data)))
			if isRetryableStatus(resp.StatusCode) && attempt < maxGETAttempts && retryAfterErr(ctx, attempt) {
				continue
			}
			return "", lastErr
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return "", fmt.Errorf("github: decode GET %s response: %w", url, err)
			}
		}
		return nextLink(link), nil
	}
	return "", lastErr
}

// linkNextRe extracts the "next" URL from a GitHub Link header, e.g.
// `<https://api.github.com/…&page=2>; rel="next", <…>; rel="last"`.
var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// nextLink returns "" when header carries no rel="next" (the last page).
func nextLink(header string) string {
	if header == "" {
		return ""
	}
	if m := linkNextRe.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return ""
}

// closingKeywordRe matches GitHub's own PR-closes-issue grammar
// (close/closes/closed/fix/fixes/fixed/resolve/resolves/resolved + "#N") -
// same-repo references only; "owner/repo#N" cross-repo closing keywords are
// out of scope (#660).
var closingKeywordRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s*#(\d+)\b`)

// linkedIssueNumbers extracts the issue numbers pull's body closes, deduped
// in first-seen order.
func linkedIssueNumbers(pull json.RawMessage) []int {
	var p struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(pull, &p); err != nil {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, m := range closingKeywordRe.FindAllStringSubmatch(p.Body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// sanitizeCheckName turns a check run's display name (often containing
// spaces or parens, e.g. "build (ubuntu-latest)") into a safe filename
// component.
func sanitizeCheckName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "check"
	}
	return b.String()
}

// writeIndentedRaw pretty-prints raw (a single GitHub object's exact bytes)
// and writes it to path.
func writeIndentedRaw(path string, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return fmt.Errorf("github: indent %s: %w", path, err)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// writeIndentedValue marshals v (a []json.RawMessage array or a wrapper map)
// and writes it to path.
func writeIndentedValue(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("github: marshal %s: %w", path, err)
	}
	return os.WriteFile(path, b, 0o644)
}

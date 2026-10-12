// Input artifacts: GitHub API responses too large or raw for the inline envelope, stored via
// Host.WriteArtifact; the envelope carries only a manifest and workers use read_artifact.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const (
	maxPRFiles   = 3000
	maxPRCommits = 250
)

// artifactEntry names one input artifact recorded in a dispatch's manifest.
type artifactEntry struct {
	Name     string // artifact-local name ("comments", "event", ...), as Host.ReadArtifact/WriteArtifact take it
	Revision int64
	Changed  bool
	Note     string // one-line human summary rendered in the manifest
}

// inputArtifactKind is the recordstore kind quack saves input artifacts under; the SDK carries a bare
// Name, so ID restores the prefix read_artifact needs.
const inputArtifactKind = "bytes"

// ID is the id read_artifact accepts; the manifest must render this, not Name, or read_artifact 404s.
func (e artifactEntry) ID() string { return inputArtifactKind + ":" + e.Name }

// writeArtifact stores data via host.WriteArtifact and returns its manifest entry, nil when unavailable
// or failed (fail-soft). user must equal this dispatch's ChatRef.User.
func writeArtifact(host sdk.Host, chatID, user, name, mime string, data []byte, note string) *artifactEntry {
	if host.WriteArtifact == nil {
		return nil
	}
	rev, changed, err := host.WriteArtifact(chatID, user, name, mime, data)
	if err != nil {
		slog.Warn("github: write input artifact failed; skipping", "component", "github", "artifact", name, "err", err)
		return nil
	}
	return &artifactEntry{Name: name, Revision: rev, Changed: changed, Note: note}
}

// ContextRequest identifies the GitHub thread and which conditional artifacts apply.
type ContextRequest struct {
	Owner, Repo string
	Number      int
	IsPR        bool
	CheckSHA    string // commit whose check runs to dump; "" skips the check-runs artifact
}

// writeInputArtifacts fetches every endpoint chatID's dispatch needs and stores each as a named
// input artifact, in fixed endpoint order. Best-effort per artifact.
func (e *Extension) writeInputArtifacts(ctx context.Context, chatID, user string, req ContextRequest) []artifactEntry {
	tok, err := e.app.tokenForRepo(ctx, req.Owner, req.Repo)
	if err != nil {
		slog.Warn("github: input artifacts: could not authenticate; nothing written", "component", "github",
			"repo", req.Owner+"/"+req.Repo, "number", req.Number, "err", err)
		return nil
	}
	authz := "token " + tok

	var out []artifactEntry
	add := func(entry *artifactEntry) {
		if entry != nil {
			out = append(out, *entry)
		}
	}

	add(e.fetchAndWriteObject(ctx, chatID, user, "issue", authz, fmt.Sprintf("/repos/%s/%s/issues/%d", req.Owner, req.Repo, req.Number)))
	add(e.fetchAndWriteList(ctx, chatID, user, "comments", authz,
		fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", req.Owner, req.Repo, req.Number), 0, ""))

	if !req.IsPR {
		add(e.fetchAndWriteList(ctx, chatID, user, "timeline", authz,
			fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?per_page=100", req.Owner, req.Repo, req.Number), 0, ""))
		return out
	}

	var pullRaw json.RawMessage
	if err := e.app.doJSON(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", req.Owner, req.Repo, req.Number), authz, nil, &pullRaw); err != nil {
		slog.Warn("github: input artifacts: fetch failed; skipping", "component", "github", "artifact", "pull", "err", err)
	} else {
		add(writeArtifact(e.host, chatID, user, "pull", "application/json", pullRaw, "1 object"))
	}
	add(e.fetchAndWriteList(ctx, chatID, user, "files", authz,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", req.Owner, req.Repo, req.Number),
		maxPRFiles, fmt.Sprintf("GitHub caps GET /pulls/%d/files at %d files; this PR's file list was cut off there.", req.Number, maxPRFiles)))
	add(e.fetchAndWriteList(ctx, chatID, user, "commits", authz,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100", req.Owner, req.Repo, req.Number),
		maxPRCommits, fmt.Sprintf("GitHub caps GET /pulls/%d/commits at %d commits; this PR's commit list was cut off there.", req.Number, maxPRCommits)))
	add(e.fetchAndWriteList(ctx, chatID, user, "reviews", authz,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", req.Owner, req.Repo, req.Number), 0, ""))
	add(e.fetchAndWriteList(ctx, chatID, user, "review-comments", authz,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100", req.Owner, req.Repo, req.Number), 0, ""))

	if req.CheckSHA != "" {
		runs, entry := e.fetchAndWriteCheckRuns(ctx, chatID, user, authz, req.Owner, req.Repo, req.CheckSHA)
		add(entry)
		for _, r := range runs {
			if !r.failed {
				continue
			}
			add(e.fetchAndWriteList(ctx, chatID, user, "annotations-"+sanitizeCheckName(r.name), authz,
				fmt.Sprintf("/repos/%s/%s/check-runs/%d/annotations?per_page=100", req.Owner, req.Repo, r.id), 0, ""))
		}
	}

	for _, n := range linkedIssueNumbers(pullRaw) {
		add(e.fetchAndWriteObject(ctx, chatID, user, fmt.Sprintf("linked-issue-%d", n), authz, fmt.Sprintf("/repos/%s/%s/issues/%d", req.Owner, req.Repo, n)))
		add(e.fetchAndWriteList(ctx, chatID, user, fmt.Sprintf("linked-issue-%d-comments", n), authz,
			fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", req.Owner, req.Repo, n), 0, ""))
	}
	return out
}

// fetchAndWriteObject fetches one object endpoint and stores it verbatim as
// a blob artifact. nil + WARN on failure (fail-soft).
func (e *Extension) fetchAndWriteObject(ctx context.Context, chatID, user, name, authz, path string) *artifactEntry {
	var raw json.RawMessage
	if err := e.app.doJSON(ctx, http.MethodGet, path, authz, nil, &raw); err != nil {
		slog.Warn("github: input artifacts: fetch failed; skipping", "component", "github", "artifact", name, "err", err)
		return nil
	}
	return writeArtifact(e.host, chatID, user, name, "application/json", raw, "1 object")
}

// fetchAndWriteList fetches a list endpoint to exhaustion and stores it as a blob artifact.
func (e *Extension) fetchAndWriteList(ctx context.Context, chatID, user, name, authz, firstPath string, cap int, capNote string) *artifactEntry {
	items, truncated, err := e.app.fetchAllPages(ctx, firstPath, authz, cap)
	if err != nil {
		slog.Warn("github: input artifacts: fetch failed; skipping", "component", "github", "artifact", name, "err", err)
		return nil
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	var payload any = items
	note := fmt.Sprintf("%d items", len(items))
	if truncated {
		payload = map[string]any{"items": items, "truncated": true, "note": capNote}
		note = capNote
	}
	b, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("github: input artifacts: marshal failed; skipping", "component", "github", "artifact", name, "err", err)
		return nil
	}
	return writeArtifact(e.host, chatID, user, name, "application/json", b, note)
}

// fetchAndWriteCheckRuns stores the "check-runs" artifact; the endpoint wraps its array,
// so fetchAndWriteList can't serve it.
func (e *Extension) fetchAndWriteCheckRuns(ctx context.Context, chatID, user, authz, owner, repo, sha string) ([]checkRunSummary, *artifactEntry) {
	items, err := e.app.fetchCheckRuns(ctx, authz, owner, repo, sha)
	if err != nil {
		slog.Warn("github: input artifacts: fetch failed; skipping", "component", "github", "artifact", "check-runs", "err", err)
		return nil, nil
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	out := make([]checkRunSummary, 0, len(items))
	var failed int
	for _, raw := range items {
		var r struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		f := r.Conclusion == "failure" || r.Conclusion == "timed_out"
		if f {
			failed++
		}
		out = append(out, checkRunSummary{id: r.ID, name: r.Name, failed: f})
	}
	payload := map[string]any{"total_count": len(items), "check_runs": items}
	b, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("github: input artifacts: marshal failed; skipping", "component", "github", "artifact", "check-runs", "err", err)
		return out, nil
	}
	note := fmt.Sprintf("%d checks, %d failed", len(items), failed)
	return out, writeArtifact(e.host, chatID, user, "check-runs", "application/json", b, note)
}

type checkRunSummary struct {
	id     int64
	name   string
	failed bool
}

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

// fetchAllPages follows a list endpoint's Link header to exhaustion. Reports truncated when cap is hit.
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

// doPagedGET performs one authenticated GET and returns the Link header's next page URL;
// a.http's transport retries.
func (a *App) doPagedGET(ctx context.Context, pathOrURL, authz string, out any) (string, error) {
	url := pathOrURL
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = a.apiBase + pathOrURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: GET %s: %w", url, err)
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	link := resp.Header.Get("Link")
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github: GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return "", fmt.Errorf("github: decode GET %s response: %w", url, err)
		}
	}
	return nextLink(link), nil
}

var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(header string) string {
	if m := linkNextRe.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return ""
}

// closingKeywordRe matches GitHub's close/fix/resolve + "#N" grammar for same-repo references.
var closingKeywordRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s*#(\d+)\b`)

// linkedIssueNumbers extracts issue numbers a PR body closes, deduped in first-seen order.
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

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testContextApp builds an App pointed at srv with a pre-cached installation
// token for owner/repo "acme/widgets" - the same shortcut app_test.go uses to
// skip the JWT/App-auth dance in unit tests.
func testContextApp(srv *httptest.Server) *App {
	return &App{
		apiBase:  srv.URL,
		http:     srv.Client(),
		installs: map[string]int64{"acme/widgets": 1},
		tokens:   map[int64]cachedToken{1: {token: "ghs_x", expires: time.Now().Add(time.Hour)}},
	}
}

// pagedArrayHandler serves a bare JSON array of `total` {"id":N} objects,
// paginated at perPage with a Link: rel="next" header on every page but the
// last - the real GitHub pagination shape WriteContextDir follows.
func pagedArrayHandler(total, perPage int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		start := (page - 1) * perPage
		end := min(start+perPage, total)
		var items []string
		for i := start; i < end; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d}`, i+1))
		}
		if end < total {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	}
}

func readJSONFile(t *testing.T, dir, name string, v any) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read %s: %v", name, err)
	}
	if v != nil {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	return true
}

// TestWriteContextDirPaginatesCommentsFully is test case 1 (#660): a partial
// page that LOOKS complete is the failure mode this file exists to kill -
// comments.json must carry every comment across every page, not just the first.
func TestWriteContextDirPaginatesCommentsFully(t *testing.T) {
	const total = 250 // 3 pages at per_page=100: 100 + 100 + 50
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/5", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":5,"title":"t","body":"b"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/5/comments", pagedArrayHandler(total, 100))
	mux.HandleFunc("/repos/acme/widgets/issues/5/timeline", pagedArrayHandler(0, 100))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := testContextApp(srv)
	dir := t.TempDir()
	if err := app.WriteContextDir(context.Background(), dir, ContextRequest{Owner: "acme", Repo: "widgets", Number: 5}); err != nil {
		t.Fatalf("WriteContextDir: %v", err)
	}

	var comments []json.RawMessage
	if !readJSONFile(t, dir, "comments.json", &comments) {
		t.Fatal("comments.json was not written")
	}
	if len(comments) != total {
		t.Errorf("comments.json has %d items; want %d (pagination not exhausted)", len(comments), total)
	}
}

// TestWriteContextDirRecordsGitHubCommitCap is test case 2: when the commit
// count reaches GitHub's own 250-commit ceiling, commits.json must say so
// explicitly rather than silently looking like a complete list.
func TestWriteContextDirRecordsGitHubCommitCap(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"b","pull_request":{}}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/9/comments", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"no closing keyword here"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/9/files", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/commits", pagedArrayHandler(maxPRCommits, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/reviews", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/comments", pagedArrayHandler(0, 100))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := testContextApp(srv)
	dir := t.TempDir()
	if err := app.WriteContextDir(context.Background(), dir, ContextRequest{Owner: "acme", Repo: "widgets", Number: 9, IsPR: true}); err != nil {
		t.Fatalf("WriteContextDir: %v", err)
	}

	var wrapped struct {
		Items     []json.RawMessage `json:"items"`
		Truncated bool              `json:"truncated"`
		Note      string            `json:"note"`
	}
	if !readJSONFile(t, dir, "commits.json", &wrapped) {
		t.Fatal("commits.json was not written")
	}
	if !wrapped.Truncated {
		t.Error("commits.json: truncated = false; want true (hit GitHub's 250-commit cap)")
	}
	if len(wrapped.Items) != maxPRCommits {
		t.Errorf("commits.json has %d items; want exactly the %d-commit cap", len(wrapped.Items), maxPRCommits)
	}
	if wrapped.Note == "" {
		t.Error("commits.json: note is empty; the cap must be stated explicitly")
	}
}

// TestWriteContextDirUnderCapIsNotMarkedTruncated proves the truncated flag
// is not a blanket default: a normal PR well under the commit cap gets a bare
// array, matching GitHub's own untruncated response shape.
func TestWriteContextDirUnderCapIsNotMarkedTruncated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"b","pull_request":{}}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/9/comments", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"nothing to close"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/9/files", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/commits", pagedArrayHandler(5, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/reviews", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/comments", pagedArrayHandler(0, 100))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := testContextApp(srv)
	dir := t.TempDir()
	if err := app.WriteContextDir(context.Background(), dir, ContextRequest{Owner: "acme", Repo: "widgets", Number: 9, IsPR: true}); err != nil {
		t.Fatalf("WriteContextDir: %v", err)
	}

	var items []json.RawMessage
	if !readJSONFile(t, dir, "commits.json", &items) {
		t.Fatal("commits.json was not written, or was written wrapped instead of a bare array")
	}
	if len(items) != 5 {
		t.Errorf("commits.json has %d items; want 5", len(items))
	}
}

// TestWriteContextDirFailsSoftPerEndpoint is test case 3: one failing
// endpoint (files.json, 500s every request) must not abort the others, and
// must not leave behind an empty file that reads as "no data" when the truth
// is "never fetched".
func TestWriteContextDirFailsSoftPerEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"b","pull_request":{}}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/9/comments", pagedArrayHandler(1, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"nothing to close"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/9/files", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/9/commits", pagedArrayHandler(1, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/reviews", pagedArrayHandler(1, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/comments", pagedArrayHandler(1, 100))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := testContextApp(srv)
	dir := t.TempDir()
	if err := app.WriteContextDir(context.Background(), dir, ContextRequest{Owner: "acme", Repo: "widgets", Number: 9, IsPR: true}); err != nil {
		t.Fatalf("WriteContextDir: %v", err)
	}

	if readJSONFile(t, dir, "files.json", nil) {
		t.Error("files.json was written despite every fetch failing - a failed endpoint must never produce a file")
	}
	for _, want := range []string{"issue.json", "comments.json", "pull.json", "commits.json", "reviews.json", "review-comments.json"} {
		if !readJSONFile(t, dir, want, nil) {
			t.Errorf("%s was not written - one failing endpoint (files.json) must not abort the rest", want)
		}
	}
}

// TestWriteContextDirIssueThreadWritesOnlyIssueFiles pins the per-trigger
// table: an issue thread (IsPR=false) never gets PR-only files, and gets
// timeline.json instead.
func TestWriteContextDirIssueThreadWritesOnlyIssueFiles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/3", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":3,"title":"t","body":"b"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/3/comments", pagedArrayHandler(2, 100))
	mux.HandleFunc("/repos/acme/widgets/issues/3/timeline", pagedArrayHandler(2, 100))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := testContextApp(srv)
	dir := t.TempDir()
	if err := app.WriteContextDir(context.Background(), dir, ContextRequest{Owner: "acme", Repo: "widgets", Number: 3, IsPR: false}); err != nil {
		t.Fatalf("WriteContextDir: %v", err)
	}
	for _, want := range []string{"issue.json", "comments.json", "timeline.json"} {
		if !readJSONFile(t, dir, want, nil) {
			t.Errorf("%s was not written", want)
		}
	}
	for _, notWant := range []string{"pull.json", "files.json", "commits.json", "reviews.json", "review-comments.json"} {
		if readJSONFile(t, dir, notWant, nil) {
			t.Errorf("%s was written for an issue thread; PR-only files must not appear", notWant)
		}
	}
}

// TestWriteContextDirLinkedIssue pins the closing-keyword table entry:
// linked-issue-{n}.json and linked-issue-{n}-comments.json appear only when
// the PR body carries a GitHub closing keyword.
func TestWriteContextDirLinkedIssue(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"b","pull_request":{}}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/9/comments", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"This closes #42 for good."}`)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/9/files", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/commits", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/reviews", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/comments", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/issues/42", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":42,"title":"the linked issue","body":"original ask"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/42/comments", pagedArrayHandler(3, 100))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := testContextApp(srv)
	dir := t.TempDir()
	if err := app.WriteContextDir(context.Background(), dir, ContextRequest{Owner: "acme", Repo: "widgets", Number: 9, IsPR: true}); err != nil {
		t.Fatalf("WriteContextDir: %v", err)
	}
	var issue struct {
		Number int `json:"number"`
	}
	if !readJSONFile(t, dir, "linked-issue-42.json", &issue) {
		t.Fatal("linked-issue-42.json was not written")
	}
	if issue.Number != 42 {
		t.Errorf("linked-issue-42.json number = %d; want 42", issue.Number)
	}
	var comments []json.RawMessage
	if !readJSONFile(t, dir, "linked-issue-42-comments.json", &comments) {
		t.Fatal("linked-issue-42-comments.json was not written")
	}
	if len(comments) != 3 {
		t.Errorf("linked-issue-42-comments.json has %d items; want 3", len(comments))
	}
}

// TestWriteContextDirCheckRunsAndAnnotations pins the CI-trigger table entry:
// check-runs.json always appears when CheckSHA is set, and annotations-*.json
// appears ONLY for a failed check, never a passing one.
func TestWriteContextDirCheckRunsAndAnnotations(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"b","pull_request":{}}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/9/comments", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"nothing to close"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/9/files", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/commits", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/reviews", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/comments", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/commits/deadbeef/check-runs", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"total_count":2,"check_runs":[`+
			`{"id":101,"name":"build","conclusion":"failure"},`+
			`{"id":102,"name":"lint (ubuntu-latest)","conclusion":"success"}]}`)
	})
	mux.HandleFunc("/repos/acme/widgets/check-runs/101/annotations", pagedArrayHandler(2, 100))
	mux.HandleFunc("/repos/acme/widgets/check-runs/102/annotations", func(w http.ResponseWriter, r *http.Request) {
		t.Error("annotations fetched for a PASSING check run")
		fmt.Fprint(w, "[]")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := testContextApp(srv)
	dir := t.TempDir()
	req := ContextRequest{Owner: "acme", Repo: "widgets", Number: 9, IsPR: true, CheckSHA: "deadbeef"}
	if err := app.WriteContextDir(context.Background(), dir, req); err != nil {
		t.Fatalf("WriteContextDir: %v", err)
	}

	var runs struct {
		TotalCount int               `json:"total_count"`
		CheckRuns  []json.RawMessage `json:"check_runs"`
	}
	if !readJSONFile(t, dir, "check-runs.json", &runs) {
		t.Fatal("check-runs.json was not written")
	}
	if len(runs.CheckRuns) != 2 {
		t.Errorf("check-runs.json has %d runs; want 2", len(runs.CheckRuns))
	}
	if !readJSONFile(t, dir, "annotations-build.json", nil) {
		t.Error("annotations-build.json was not written for the failed \"build\" check")
	}
	if readJSONFile(t, dir, "annotations-lint--ubuntu-latest-.json", nil) {
		t.Error("annotations file was written for the passing \"lint (ubuntu-latest)\" check")
	}
}

// TestWriteContextDirNoCheckSHASkipsChecks pins the "conditional" half of the
// table: without a CheckSHA (a non-CI-triggered run) neither check-runs.json
// nor any annotations file appears at all, even for a PR thread.
func TestWriteContextDirNoCheckSHASkipsChecks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"b","pull_request":{}}`)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/9/comments", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":9,"title":"t","body":"b"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/9/files", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/commits", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/reviews", pagedArrayHandler(0, 100))
	mux.HandleFunc("/repos/acme/widgets/pulls/9/comments", pagedArrayHandler(0, 100))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := testContextApp(srv)
	dir := t.TempDir()
	if err := app.WriteContextDir(context.Background(), dir, ContextRequest{Owner: "acme", Repo: "widgets", Number: 9, IsPR: true}); err != nil {
		t.Fatalf("WriteContextDir: %v", err)
	}
	if readJSONFile(t, dir, "check-runs.json", nil) {
		t.Error("check-runs.json was written with no CheckSHA set")
	}
}

// TestWriteContextDirRequiresExistingDir pins the "caller resolves dir,
// WriteContextDir never creates it" split (mirrors internal/tools.SetupClone).
func TestWriteContextDirRequiresExistingDir(t *testing.T) {
	app := testContextApp(httptest.NewServer(http.NotFoundHandler()))
	err := app.WriteContextDir(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"),
		ContextRequest{Owner: "acme", Repo: "widgets", Number: 1})
	if err == nil {
		t.Fatal("WriteContextDir with a non-existent dir: want an error")
	}
}

// TestNextLinkParsesRelAndIgnoresOtherRels pins the Link-header parser
// pagination depends on.
func TestNextLinkParsesRelAndIgnoresOtherRels(t *testing.T) {
	cases := map[string]string{
		"": "",
		`<https://api.github.com/x?page=3>; rel="last"`: "",
		`<https://api.github.com/x?page=2>; rel="next"`: "https://api.github.com/x?page=2",
		`<https://api.github.com/x?page=1>; rel="prev", <https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`: "https://api.github.com/x?page=2",
	}
	for header, want := range cases {
		if got := nextLink(header); got != want {
			t.Errorf("nextLink(%q) = %q; want %q", header, got, want)
		}
	}
}

// TestLinkedIssueNumbersMatchesGitHubClosingKeywords pins the keyword grammar
// and the dedup/same-repo-only scope.
func TestLinkedIssueNumbersMatchesGitHubClosingKeywords(t *testing.T) {
	cases := []struct {
		body string
		want []int
	}{
		{`{"body":"Closes #42"}`, []int{42}},
		{`{"body":"fixes #1 and resolves #2"}`, []int{1, 2}},
		{`{"body":"Fixed #7, fixed #7 again"}`, []int{7}},
		{`{"body":"see #99 for context"}`, nil},       // no keyword: a reference, not a closing keyword
		{`{"body":"closes owner/other-repo#5"}`, nil}, // cross-repo: out of scope
	}
	for _, c := range cases {
		got := linkedIssueNumbers(json.RawMessage(c.body))
		if len(got) != len(c.want) {
			t.Errorf("linkedIssueNumbers(%q) = %v; want %v", c.body, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("linkedIssueNumbers(%q) = %v; want %v", c.body, got, c.want)
			}
		}
	}
}

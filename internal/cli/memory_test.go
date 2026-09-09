package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRunMemoryList(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/memories" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"memories":[
			{"id":"m1","bucket":"role:coding","content":"the deploy runs via github actions on merge to main","author":"jason","kind":"fact","status":"reinforced","timestamp":"2026-01-02T03:04:00Z"}
		],"total":1}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryList(context.Background(), &out, srv.URL, "role:coding", "", "", "", 10, false, false); err != nil {
		t.Fatalf("RunMemoryList: %v", err)
	}
	q, _ := url.ParseQuery(gotQuery)
	if q.Get("bucket") != "role:coding" || q.Get("limit") != "10" {
		t.Errorf("query = %q, want bucket=role:coding&limit=10", gotQuery)
	}
	if q.Has("include_invalidated") {
		t.Errorf("query = %q, should omit include_invalidated when false", gotQuery)
	}
	s := out.String()
	for _, want := range []string{"ID", "m1", "role:coding", "reinforced", "the deploy runs"} {
		if !strings.Contains(s, want) {
			t.Errorf("list output missing %q:\n%s", want, s)
		}
	}
}

func TestRunMemoryListSearchAndIncludeInvalidated(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"memories":[],"total":0}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryList(context.Background(), &out, srv.URL, "", "how does deploy work", "", "", 0, true, false); err != nil {
		t.Fatalf("RunMemoryList: %v", err)
	}
	q, _ := url.ParseQuery(gotQuery)
	if q.Get("q") != "how does deploy work" || q.Get("include_invalidated") != "true" {
		t.Errorf("query = %q, want q + include_invalidated=true", gotQuery)
	}
	if !strings.Contains(out.String(), "No memories match") {
		t.Errorf("empty output = %q, want the no-match message", out.String())
	}
}

func TestRunMemoryListJSON(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"memories":[{"id":"m1","bucket":"b","content":"c","author":"a","kind":"fact","timestamp":"2026-01-02T03:04:00Z"}],"total":1}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryList(context.Background(), &out, srv.URL, "", "", "", "", 0, false, true); err != nil {
		t.Fatalf("RunMemoryList: %v", err)
	}
	var decoded struct {
		Memories []struct{ Id string }
		Total    int
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	if decoded.Total != 1 || len(decoded.Memories) != 1 || decoded.Memories[0].Id != "m1" {
		t.Errorf("decoded = %+v, want one memory m1", decoded)
	}
}

// With no --limit, the CLI pages through the whole store.
func TestRunMemoryListAutoPages(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page_token") == "" {
			io.WriteString(w, `{"memories":[{"id":"m1","bucket":"b","content":"first page","author":"a","kind":"fact","timestamp":"2026-01-02T03:04:00Z"}],"total":2,"next_page_token":"p2"}`)
			return
		}
		if r.URL.Query().Get("page_token") != "p2" {
			t.Errorf("page_token = %q, want p2", r.URL.Query().Get("page_token"))
		}
		io.WriteString(w, `{"memories":[{"id":"m2","bucket":"b","content":"second page","author":"a","kind":"fact","timestamp":"2026-01-02T03:04:00Z"}],"total":2}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryList(context.Background(), &out, srv.URL, "", "", "", "", 0, false, false); err != nil {
		t.Fatalf("RunMemoryList: %v", err)
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (auto-paged)", calls)
	}
	s := out.String()
	if !strings.Contains(s, "first page") || !strings.Contains(s, "second page") {
		t.Errorf("output missing a page's memory:\n%s", s)
	}
}

// append onto a nil slice with nothing to append stays nil - an all-empty
// store must not encode its --json memories field as null.
func TestRunMemoryListAutoPageEmptyIsJSONArray(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"memories":[],"total":0}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryList(context.Background(), &out, srv.URL, "", "", "", "", 0, false, true); err != nil {
		t.Fatalf("RunMemoryList: %v", err)
	}
	var decoded struct {
		Memories []struct{ Id string } `json:"memories"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	if decoded.Memories == nil {
		t.Error("memories decoded as null (JSON), want []")
	}
	if !strings.Contains(out.String(), `"memories": []`) {
		t.Errorf("output = %s, want the literal memories: [] shape", out.String())
	}
}

// An explicit --limit must not be overridden by auto-paging.
func TestRunMemoryListExplicitLimitMakesOneRequest(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"memories":[{"id":"m1","bucket":"b","content":"c","author":"a","kind":"fact","timestamp":"2026-01-02T03:04:00Z"}],"total":5,"next_page_token":"p2"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryList(context.Background(), &out, srv.URL, "", "", "", "", 1, false, false); err != nil {
		t.Fatalf("RunMemoryList: %v", err)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want exactly 1 with an explicit --limit", calls)
	}
}

// --tier and --sort must reach the server as query params.
func TestRunMemoryListTierAndSort(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `{"memories":[],"total":0}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryList(context.Background(), &out, srv.URL, "", "", "verified", "score", 5, false, false); err != nil {
		t.Fatalf("RunMemoryList: %v", err)
	}
	q, _ := url.ParseQuery(gotQuery)
	if q.Get("tier") != "verified" || q.Get("sort") != "score" {
		t.Errorf("query = %q, want tier=verified&sort=score", gotQuery)
	}
}

func TestRunMemoryForget(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryForget(context.Background(), &out, srv.URL, "m1", "poisoned"); err != nil {
		t.Fatalf("RunMemoryForget: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/memories/m1" {
		t.Errorf("request = %s %s, want DELETE /api/v1/memories/m1", gotMethod, gotPath)
	}
	if !strings.Contains(string(gotBody), `"poisoned"`) {
		t.Errorf("body = %s, want it to carry the reason", gotBody)
	}
	if !strings.Contains(out.String(), "invalidated m1") {
		t.Errorf("output = %q, want confirmation", out.String())
	}
}

func TestRunMemoryForgetNotFound(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := RunMemoryForget(context.Background(), &out, srv.URL, "missing", "")
	if err == nil || !strings.Contains(err.Error(), "memory missing not found") {
		t.Fatalf("err = %v, want a memory-not-found message", err)
	}
}

// A direct per-id GET (exactly one server call), not a full-store page scan.
func TestRunMemoryShow(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var calls int
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotPath = r.URL.Path
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m1","bucket":"repo:r","content":"a fact worth showing","author":"a","kind":"fact",
			 "timestamp":"2026-01-02T03:04:00Z","status":"unverified","tier":"verified",
			 "upvotes":2,"downvotes":1,"vote_score":1,"recalls":3,
			 "last_upvoted_at":"2026-01-03T00:00:00Z","last_recalled_at":"2026-01-04T00:00:00Z"}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryShow(context.Background(), &out, srv.URL, "m1", false); err != nil {
		t.Fatalf("RunMemoryShow: %v", err)
	}
	if gotPath != "/api/v1/memories/m1" {
		t.Errorf("path = %q, want /api/v1/memories/m1", gotPath)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want exactly 1 (a direct per-id GET, not a page scan)", calls)
	}
	s := out.String()
	for _, want := range []string{"m1", "verified", "+2 / -1", "score 1", "recalls:  3", "a fact worth showing"} {
		if !strings.Contains(s, want) {
			t.Errorf("show output missing %q:\n%s", want, s)
		}
	}
}

func TestRunMemoryShowNotFound(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := RunMemoryShow(context.Background(), &out, srv.URL, "does-not-exist", false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

// TestRunMemorySweepPartialFailure covers the CLI regression fixed alongside
// the partial-failure REST change: a non-empty res.Errors must show up in
// human output and make the command fail, not silently exit 0.
func TestRunMemorySweepPartialFailure(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"dry_run":false,"stores":[{"store":"task","evaluated":2,"kept":1,"rules":[]}],
			"errors":[{"store":"user","message":"list: boom"}]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := RunMemorySweep(context.Background(), &out, srv.URL, false, false, false, false)
	if err == nil {
		t.Fatalf("RunMemorySweep err = nil, want a non-nil error signalling the partial failure")
	}
	s := out.String()
	for _, want := range []string{"store task: evaluated 2, kept 1", "store user failed: list: boom"} {
		if !strings.Contains(s, want) {
			t.Errorf("sweep output missing %q:\n%s", want, s)
		}
	}
}

// TestRunMemorySweepPartialFailureJSON: --as-json must still print the full
// response (including errors) and, unlike the human path, RunMemorySweep
// itself doesn't error on the JSON path since the response was decoded fine.
func TestRunMemorySweepPartialFailureJSON(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"dry_run":false,"stores":[],"errors":[{"store":"user","message":"list: boom"}]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemorySweep(context.Background(), &out, srv.URL, false, false, false, true); err != nil {
		t.Fatalf("RunMemorySweep --as-json: %v", err)
	}
	var decoded struct {
		Errors []struct {
			Store   string
			Message string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	if len(decoded.Errors) != 1 || decoded.Errors[0].Store != "user" || decoded.Errors[0].Message != "list: boom" {
		t.Errorf("decoded errors = %+v, want one user/list: boom entry", decoded.Errors)
	}
}

// TestRunMemorySweepAllStoresFail covers the reordering fix: empty Stores
// with non-empty Errors must print the failures and exit non-zero, not the
// misleading "No memory stores configured." (that early return only applies
// when Errors is also empty).
func TestRunMemorySweepAllStoresFail(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"dry_run":false,"stores":[],"errors":[{"store":"task","message":"list: boom"},{"store":"user","message":"list: boom"}]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := RunMemorySweep(context.Background(), &out, srv.URL, false, false, false, false)
	if err == nil {
		t.Fatalf("RunMemorySweep err = nil, want a non-nil error when every store failed")
	}
	s := out.String()
	if strings.Contains(s, "No memory stores configured.") {
		t.Errorf("output = %q, must not print the no-stores message when Errors is non-empty", s)
	}
	for _, want := range []string{"store task failed: list: boom", "store user failed: list: boom"} {
		if !strings.Contains(s, want) {
			t.Errorf("sweep output missing %q:\n%s", want, s)
		}
	}
}

// TestRunMemorySweepAllOK: no errors, no store-failure lines, exit clean.
func TestRunMemorySweepAllOK(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"dry_run":false,"stores":[{"store":"task","evaluated":1,"kept":1,"rules":[]}]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemorySweep(context.Background(), &out, srv.URL, false, false, false, false); err != nil {
		t.Fatalf("RunMemorySweep: %v", err)
	}
	if strings.Contains(out.String(), "failed") {
		t.Errorf("output = %q, want no failure line", out.String())
	}
}

func TestTruncateLine(t *testing.T) {
	if got := truncateLine("short", 80); got != "short" {
		t.Errorf("truncateLine short = %q", got)
	}
	if got := truncateLine("line one\nline two", 80); got != "line one line two" {
		t.Errorf("truncateLine newline-join = %q", got)
	}
	if got := truncateLine("abcdefghij", 5); got != "abcde…" {
		t.Errorf("truncateLine clip = %q", got)
	}
}

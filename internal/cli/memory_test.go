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
	if err := RunMemoryList(context.Background(), &out, srv.URL, "role:coding", "", 10, false, false); err != nil {
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
	if err := RunMemoryList(context.Background(), &out, srv.URL, "", "how does deploy work", 0, true, false); err != nil {
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
	if err := RunMemoryList(context.Background(), &out, srv.URL, "", "", 0, false, true); err != nil {
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

// TestRunMemoryShow covers the happy path: found on the first page, human
// output includes the vote/tier/recall fields.
func TestRunMemoryShow(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"memories":[
			{"id":"m1","bucket":"repo:r","content":"a fact worth showing","author":"a","kind":"fact",
			 "timestamp":"2026-01-02T03:04:00Z","status":"unverified","tier":"verified",
			 "upvotes":2,"downvotes":1,"vote_score":1,"recalls":3,
			 "last_upvoted_at":"2026-01-03T00:00:00Z","last_recalled_at":"2026-01-04T00:00:00Z"}
		],"total":1}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryShow(context.Background(), &out, srv.URL, "m1", false); err != nil {
		t.Fatalf("RunMemoryShow: %v", err)
	}
	s := out.String()
	for _, want := range []string{"m1", "verified", "+2 / -1", "score 1", "recalls:  3", "a fact worth showing"} {
		if !strings.Contains(s, want) {
			t.Errorf("show output missing %q:\n%s", want, s)
		}
	}
}

// TestRunMemoryShowPaging covers findMemory's page_token loop: the id lands
// on the second page, reached via the first response's next_page_token.
func TestRunMemoryShowPaging(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page_token") == "" {
			io.WriteString(w, `{"memories":[{"id":"other","bucket":"repo:r","content":"c","author":"a","kind":"fact","timestamp":"2026-01-02T03:04:00Z"}],"total":2,"next_page_token":"p2"}`)
			return
		}
		if r.URL.Query().Get("page_token") != "p2" {
			t.Errorf("page_token = %q, want p2", r.URL.Query().Get("page_token"))
		}
		io.WriteString(w, `{"memories":[{"id":"m2","bucket":"repo:r","content":"on page two","author":"a","kind":"fact","timestamp":"2026-01-02T03:04:00Z"}],"total":2}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemoryShow(context.Background(), &out, srv.URL, "m2", false); err != nil {
		t.Fatalf("RunMemoryShow: %v", err)
	}
	if !strings.Contains(out.String(), "on page two") {
		t.Errorf("show output = %q, want the second page's memory", out.String())
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want exactly 2 (one per page)", calls)
	}
}

// TestRunMemoryShowNotFound covers termination: no next_page_token ends the
// scan, and an id never seen across all pages is a not-found error, not an
// infinite loop.
func TestRunMemoryShowNotFound(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"memories":[{"id":"m1","bucket":"repo:r","content":"c","author":"a","kind":"fact","timestamp":"2026-01-02T03:04:00Z"}],"total":1}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := RunMemoryShow(context.Background(), &out, srv.URL, "does-not-exist", false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want exactly 1 (no next_page_token, must not loop)", calls)
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
	err := RunMemorySweep(context.Background(), &out, srv.URL, false, false)
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
	if err := RunMemorySweep(context.Background(), &out, srv.URL, false, true); err != nil {
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

// TestRunMemorySweepAllOK: no errors, no store-failure lines, exit clean.
func TestRunMemorySweepAllOK(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"dry_run":false,"stores":[{"store":"task","evaluated":1,"kept":1,"rules":[]}]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunMemorySweep(context.Background(), &out, srv.URL, false, false); err != nil {
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

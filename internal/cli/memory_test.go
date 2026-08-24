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

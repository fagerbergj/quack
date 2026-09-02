package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunArtifactList(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/chats/c1/artifacts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[
			{"name":"scan.png","revisions":[
				{"revision":1,"mime_type":"image/png","size":1024},
				{"revision":2,"mime_type":"image/png","size":2048}
			]}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunArtifactList(context.Background(), &out, srv.URL, "c1", false); err != nil {
		t.Fatalf("RunArtifactList: %v", err)
	}
	s := out.String()
	for _, want := range []string{"NAME", "scan.png", "2", "2.0KB", "image/png"} {
		if !strings.Contains(s, want) {
			t.Errorf("list output missing %q:\n%s", want, s)
		}
	}
}

func TestRunArtifactListEmpty(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunArtifactList(context.Background(), &out, srv.URL, "c1", false); err != nil {
		t.Fatalf("RunArtifactList: %v", err)
	}
	if !strings.Contains(out.String(), "No artifacts") {
		t.Errorf("empty output = %q, want the no-artifacts message", out.String())
	}
}

func TestRunArtifactListChatNotFound(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := RunArtifactList(context.Background(), &out, srv.URL, "missing", false)
	if err == nil || !strings.Contains(err.Error(), "chat missing not found") {
		t.Fatalf("err = %v, want a chat-not-found message", err)
	}
}

func TestRunArtifactDownload(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	const bytesWant = "fake-png-bytes"
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/chats/c1/artifacts/scan.png" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, bytesWant)
	}))
	defer srv.Close()

	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.png")
	var out bytes.Buffer
	if err := RunArtifactDownload(context.Background(), &out, srv.URL, "c1", "scan.png", 2, outFile); err != nil {
		t.Fatalf("RunArtifactDownload: %v", err)
	}
	if gotQuery != "revision=2" {
		t.Errorf("query = %q, want revision=2", gotQuery)
	}
	if !strings.Contains(out.String(), outFile) {
		t.Errorf("printed path = %q, want it to contain %q", out.String(), outFile)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != bytesWant {
		t.Errorf("file content = %q, want %q", got, bytesWant)
	}
}

// TestRunArtifactDownloadDefaultFilename covers the -o-less path: default
// filename is the artifact's own name in the current directory, latest
// revision (no ?revision= query param).
func TestRunArtifactDownloadDefaultFilename(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, "bytes")
	}))
	defer srv.Close()

	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer os.Chdir(cwd)

	var out bytes.Buffer
	if err := RunArtifactDownload(context.Background(), &out, srv.URL, "c1", "notes.txt", 0, ""); err != nil {
		t.Fatalf("RunArtifactDownload: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want no revision param for latest", gotQuery)
	}
	if !strings.Contains(out.String(), "notes.txt") {
		t.Errorf("printed path = %q, want it to contain notes.txt", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Errorf("default output file not written: %v", err)
	}
}

func TestRunArtifactDownloadNotFound(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.png")
	var out bytes.Buffer
	err := RunArtifactDownload(context.Background(), &out, srv.URL, "c1", "missing.png", 0, outFile)
	if err == nil || !strings.Contains(err.Error(), `no artifact "missing.png"`) {
		t.Fatalf("err = %v, want a no-artifact message", err)
	}
	if _, statErr := os.Stat(outFile); !os.IsNotExist(statErr) {
		t.Errorf("expected no partial file left behind, got stat err = %v", statErr)
	}
}

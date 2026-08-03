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

func TestRunRecordingList(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/recordings" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[
			{"chat_id":"c1","size_bytes":2048,"modified_at":"2026-01-02T03:04:00Z"},
			{"chat_id":"c2","size_bytes":512,"modified_at":"2026-01-01T00:00:00Z"}
		]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunRecordingList(context.Background(), &out, srv.URL, false); err != nil {
		t.Fatalf("RunRecordingList: %v", err)
	}
	s := out.String()
	for _, want := range []string{"CHAT ID", "c1", "c2", "2.0KB", "512B"} {
		if !strings.Contains(s, want) {
			t.Errorf("list output missing %q:\n%s", want, s)
		}
	}
}

func TestRunRecordingListEmpty(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := RunRecordingList(context.Background(), &out, srv.URL, false); err != nil {
		t.Fatalf("RunRecordingList: %v", err)
	}
	if !strings.Contains(out.String(), "No recordings yet") {
		t.Errorf("empty output = %q, want the no-recordings message", out.String())
	}
}

func TestRunRecordingListDisabled(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "recording is not enabled", http.StatusNotFound)
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := RunRecordingList(context.Background(), &out, srv.URL, false)
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("err = %v, want a not-enabled message", err)
	}
}

func TestRunRecordingExport(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	const bundleBytes = "PK\x03\x04fake-zip-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/chats/c1/recording" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/zip")
		io.WriteString(w, bundleBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	outFile := filepath.Join(dir, "c1.zip")
	var out bytes.Buffer
	if err := RunRecordingExport(context.Background(), &out, srv.URL, "c1", outFile); err != nil {
		t.Fatalf("RunRecordingExport: %v", err)
	}
	if !strings.Contains(out.String(), outFile) {
		t.Errorf("printed path = %q, want it to contain %q", out.String(), outFile)
	}
	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != bundleBytes {
		t.Errorf("file content = %q, want %q", got, bundleBytes)
	}
}

// TestRunRecordingExportDefaultFilename covers the -o-less path: default
// filename is <chat-id>.zip in the current directory.
func TestRunRecordingExportDefaultFilename(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "zip-bytes")
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
	if err := RunRecordingExport(context.Background(), &out, srv.URL, "chat-42", ""); err != nil {
		t.Fatalf("RunRecordingExport: %v", err)
	}
	if !strings.Contains(out.String(), "chat-42.zip") {
		t.Errorf("printed path = %q, want it to contain chat-42.zip", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "chat-42.zip")); err != nil {
		t.Errorf("default output file not written: %v", err)
	}
}

func TestRunRecordingExportNotFound(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no recording for this chat", http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.zip")
	var out bytes.Buffer
	err := RunRecordingExport(context.Background(), &out, srv.URL, "missing", outFile)
	if err == nil || !strings.Contains(err.Error(), "no recording for chat missing") {
		t.Fatalf("err = %v, want a no-recording message", err)
	}
	if _, statErr := os.Stat(outFile); !os.IsNotExist(statErr) {
		t.Errorf("expected no partial file left behind, got stat err = %v", statErr)
	}
}

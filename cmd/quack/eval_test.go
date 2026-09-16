package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveBundle_LocalFile: an existing local path is used as-is, with a
// no-op cleanup.
func TestResolveBundle_LocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.zip")
	if err := os.WriteFile(path, []byte("fake zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, cleanup, err := resolveBundle(context.Background(), "", path)
	if err != nil {
		t.Fatalf("resolveBundle: %v", err)
	}
	if got != path {
		t.Errorf("got %q, want the local path unchanged (%q)", got, path)
	}
	cleanup() // must not remove the caller's own file
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("cleanup removed the caller-owned local file: %v", statErr)
	}
}

// TestResolveBundle_FetchesChatIDFromServer: an argument that ISN'T a local
// file is treated as a chat id and fetched from sourceServer's recording
// endpoint into a temp file, which cleanup then removes.
func TestResolveBundle_FetchesChatIDFromServer(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/zip")
		w.Write([]byte("the bundle bytes"))
	}))
	defer srv.Close()

	path, cleanup, err := resolveBundle(context.Background(), srv.URL, "chat-abc123")
	if err != nil {
		t.Fatalf("resolveBundle: %v", err)
	}
	defer cleanup()
	if gotPath != "/api/v1/chats/chat-abc123/recording" {
		t.Errorf("fetched path = %q, want the recording endpoint", gotPath)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fetched bundle: %v", err)
	}
	if string(b) != "the bundle bytes" {
		t.Errorf("bundle contents = %q, want the fetched bytes", b)
	}

	cleanup()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("cleanup did not remove the temp bundle file")
	}
}

// TestResolveBundle_FetchFailure: a chat id the server has no recording for
// (404 / ErrNotFound) surfaces as a clear error, not a panic on an empty body.
func TestResolveBundle_FetchFailure(t *testing.T) {
	t.Setenv("QUACK_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := resolveBundle(context.Background(), srv.URL, "no-such-chat")
	if err == nil {
		t.Fatal("want an error for a chat id with no recording")
	}
}

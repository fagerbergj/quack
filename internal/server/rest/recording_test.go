package rest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
)

// TestGetChatRecordingNoStore: recording disabled entirely (nil ledgerStore)
// 404s.
func TestGetChatRecordingNoStore(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/c1/recording", nil)
	h.GetChatRecording(rec, req, "c1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetChatRecordingNoSession: a store is configured but this chat was
// never recorded - still 404, not a 500 or a truncated 200.
func TestGetChatRecordingNoSession(t *testing.T) {
	h := newTestHandler(t)
	store, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	h.ledgerStore = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/never-recorded/recording", nil)
	h.GetChatRecording(rec, req, "never-recorded")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetChatRecordingRoundTrip: a recorded chat's bundle downloads as a
// valid ZIP with the expected manifest and entries content.
func TestGetChatRecordingRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	store, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	h.ledgerStore = store
	h.quackVersion = "v9.9.9"

	ctx := context.Background()
	if err := store.Append(ctx, "c1", []byte(`{"seq":1}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(ctx, "c1", []byte(`{"seq":2}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/c1/recording", nil)
	h.GetChatRecording(rec, req, "c1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}

	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	mf, ok := files["manifest.json"]
	if !ok {
		t.Fatalf("bundle missing manifest.json")
	}
	rc, err := mf.Open()
	if err != nil {
		t.Fatalf("open manifest.json: %v", err)
	}
	defer rc.Close()
	var m ledger.Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.QuackVersion != "v9.9.9" {
		t.Errorf("QuackVersion = %q, want v9.9.9", m.QuackVersion)
	}
	if m.SessionID != "c1" {
		t.Errorf("SessionID = %q, want c1", m.SessionID)
	}

	ef, ok := files["entries.jsonl"]
	if !ok {
		t.Fatalf("bundle missing entries.jsonl")
	}
	erc, err := ef.Open()
	if err != nil {
		t.Fatalf("open entries.jsonl: %v", err)
	}
	defer erc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(erc); err != nil {
		t.Fatalf("read entries.jsonl: %v", err)
	}
	want := "{\"seq\":1}\n{\"seq\":2}\n"
	if buf.String() != want {
		t.Errorf("entries.jsonl = %q, want %q", buf.String(), want)
	}
}

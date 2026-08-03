package rest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/schema"
)

// TestListRecordingsNoStore: recording disabled entirely (nil ledgerStore)
// 404s, same disabled signal as GetChatRecording.
func TestListRecordingsNoStore(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/recordings", nil)
	h.ListRecordings(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestListRecordingsEmpty: a store is configured but nothing has been
// recorded yet - 200 with an empty array, not a 404.
func TestListRecordingsEmpty(t *testing.T) {
	h := newTestHandler(t)
	store, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	h.ledgerStore = store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/recordings", nil)
	h.ListRecordings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out schema.RecordingList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Data == nil || len(out.Data) != 0 {
		t.Errorf("data = %#v, want empty (non-nil) array", out.Data)
	}
}

// TestListRecordingsWithSessions: lists every recorded session's id and size.
func TestListRecordingsWithSessions(t *testing.T) {
	h := newTestHandler(t)
	store, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	h.ledgerStore = store
	ctx := context.Background()
	if err := store.Append(ctx, "c1", []byte(`{"seq":1}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(ctx, "c2", []byte(`{"seq":1}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/recordings", nil)
	h.ListRecordings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out schema.RecordingList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("data = %#v, want 2 entries", out.Data)
	}
	seen := map[string]bool{}
	for _, r := range out.Data {
		seen[r.ChatId] = true
		if r.SizeBytes <= 0 {
			t.Errorf("chat %s SizeBytes = %d, want > 0", r.ChatId, r.SizeBytes)
		}
	}
	if !seen["c1"] || !seen["c2"] {
		t.Errorf("data = %#v, want c1 and c2", out.Data)
	}
}

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

// A hostile chat id must never reach Content-Disposition verbatim (quack
// review on #611: header-parameter injection via `;` / quotes).
func TestGetChatRecording_SanitizesContentDisposition(t *testing.T) {
	h := newTestHandler(t)
	store, err := ledger.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	h.ledgerStore = store
	hostile := `evil"; dummy="x`
	if err := store.Append(context.Background(), hostile, []byte(`{"seq":1}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/x/recording", nil)
	h.GetChatRecording(rec, req, hostile)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	if strings.Contains(cd, `dummy=`) && !strings.Contains(cd, `filename*`) {
		if _, params, perr := mime.ParseMediaType(cd); perr != nil || params["dummy"] != "" {
			t.Fatalf("Content-Disposition not sanitized: %q", cd)
		}
	}
	if _, params, perr := mime.ParseMediaType(cd); perr != nil || params["filename"] != hostile+".zip" {
		t.Fatalf("round-trip parse failed: %q params=%v err=%v", cd, params, perr)
	}
}

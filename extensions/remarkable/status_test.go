package remarkable

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleStatusJSONUnchanged(t *testing.T) {
	e, _, _ := newTestExtension(t)
	e.st.Documents["doc-1"] = docState{
		ID: "doc-1", Name: "Meeting Notes", Folder: "Inbox",
		LastModified: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		LastOutcome:  "done", Attempts: 1,
	}

	req := httptest.NewRequest("GET", "/status.json", nil)
	rec := httptest.NewRecorder()
	e.handleStatusJSON(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"doc-1"`) || !strings.Contains(body, `"last_outcome":"done"`) {
		t.Errorf("body missing expected fields: %s", body)
	}
}

func TestHandleStatusRendersKitLinkedHTML(t *testing.T) {
	e, _, _ := newTestExtension(t)
	e.st.Documents["doc-1"] = docState{
		ID: "doc-1", Name: "Meeting Notes", Folder: "Inbox",
		LastModified: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		LastOutcome:  "done", Attempts: 1,
	}
	e.st.Documents["doc-2"] = docState{
		ID: "doc-2", Name: "Bad Scan",
		LastOutcome: "failed", LastError: "pdf decode failed", Attempts: 3,
	}
	e.st.Documents["doc-3"] = docState{
		ID: "doc-3", Name: "In Progress", InFlight: true,
	}

	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	e.handleStatus(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `href="/assets/ext/v1/kit.css"`) {
		t.Errorf("page doesn't link the v1 kit: %s", body)
	}
	if !strings.Contains(body, `href="/remarkable/status.json"`) {
		t.Errorf("page doesn't link the JSON split: %s", body)
	}
	for _, want := range []string{
		"Meeting Notes", `qk-badge--ok">done`,
		"Bad Scan", `qk-badge--err">failed`, "pdf decode failed",
		"In Progress", `qk-badge--warn">in flight`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestHandleStatusEscapesDocumentName(t *testing.T) {
	e, _, _ := newTestExtension(t)
	e.st.Documents["doc-1"] = docState{ID: "doc-1", Name: `<script>alert(1)</script>`}

	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	e.handleStatus(rec, req)

	if strings.Contains(rec.Body.String(), "<script>alert") {
		t.Errorf("document name rendered unescaped: %s", rec.Body.String())
	}
}

func TestHandleStatusNoDocumentsShowsEmptyState(t *testing.T) {
	e, _, _ := newTestExtension(t)

	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	e.handleStatus(rec, req)

	if !strings.Contains(rec.Body.String(), "No documents dispatched yet.") {
		t.Errorf("empty state missing: %s", rec.Body.String())
	}
}

package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/schema"
)

func TestListChatArtifacts_Empty(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts", nil)
	rec := httptest.NewRecorder()
	h.ListChatArtifacts(rec, req, chatID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out schema.ArtifactList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 0 {
		t.Errorf("Data = %+v, want empty", out.Data)
	}
}

func TestListChatArtifacts_UnknownChat_404(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/nope/artifacts", nil)
	rec := httptest.NewRecorder()
	h.ListChatArtifacts(rec, req, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// saveTestArtifact saves one revision directly through the handler's
// artifact service, stamping turnID exactly like SendChatMessage's
// saveAttachment would - the test-side equivalent of an upload.
func saveTestArtifact(t *testing.T, h *Handler, userID, chatID, turnID, name, mimeType string, data []byte) {
	t.Helper()
	if _, err := h.artifacts.SaveForTurn(context.Background(), &artifact.SaveRequest{
		AppName: artifactref.AppName, UserID: userID, SessionID: chatID, FileName: name,
		Part: &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: mimeType}},
	}, turnID); err != nil {
		t.Fatalf("SaveForTurn %s: %v", name, err)
	}
}

func TestListChatArtifacts_ListsRevisionsWithTurnAndMime(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)

	saveTestArtifact(t, h, userID, chatID, "turn-1", "doc.pdf", "application/pdf", []byte("v1 bytes"))
	saveTestArtifact(t, h, userID, chatID, "turn-2", "doc.pdf", "application/pdf", []byte("v2 bytes, longer"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts", nil)
	rec := httptest.NewRecorder()
	h.ListChatArtifacts(rec, req, chatID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var out schema.ArtifactList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].Name != "doc.pdf" {
		t.Fatalf("Data = %+v, want one artifact named doc.pdf", out.Data)
	}
	revs := out.Data[0].Revisions
	if len(revs) != 2 {
		t.Fatalf("Revisions = %+v, want 2", revs)
	}
	if revs[0].Revision != 1 || revs[0].TurnId == nil || *revs[0].TurnId != "turn-1" {
		t.Errorf("revision 1 = %+v, want revision=1 turn_id=turn-1", revs[0])
	}
	if revs[1].Revision != 2 || revs[1].TurnId == nil || *revs[1].TurnId != "turn-2" {
		t.Errorf("revision 2 = %+v, want revision=2 turn_id=turn-2", revs[1])
	}
	if revs[0].MimeType != "application/pdf" || revs[1].Size != int64(len("v2 bytes, longer")) {
		t.Errorf("mime/size not carried through: %+v", revs)
	}
}

func TestGetChatArtifact_LatestByDefault(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)

	saveTestArtifact(t, h, userID, chatID, "turn-1", "doc.pdf", "application/pdf", []byte("old"))
	saveTestArtifact(t, h, userID, chatID, "turn-2", "doc.pdf", "application/pdf", []byte("new"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/doc.pdf", nil)
	rec := httptest.NewRecorder()
	h.GetChatArtifact(rec, req, chatID, "doc.pdf", schema.GetChatArtifactParams{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "new" {
		t.Errorf("body = %q, want the latest revision %q", rec.Body.String(), "new")
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename=doc.pdf` {
		t.Errorf("Content-Disposition = %q, want a forced download for a non-image mime", got)
	}
}

func TestGetChatArtifact_ExplicitRevision(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)

	saveTestArtifact(t, h, userID, chatID, "turn-1", "doc.pdf", "application/pdf", []byte("old"))
	saveTestArtifact(t, h, userID, chatID, "turn-2", "doc.pdf", "application/pdf", []byte("new"))

	one := 1
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/doc.pdf?revision=1", nil)
	rec := httptest.NewRecorder()
	h.GetChatArtifact(rec, req, chatID, "doc.pdf", schema.GetChatArtifactParams{Revision: &one})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "old" {
		t.Errorf("body = %q, want revision 1's bytes %q", rec.Body.String(), "old")
	}
}

func TestGetChatArtifact_UnknownName_404(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/nope.txt", nil)
	rec := httptest.NewRecorder()
	h.GetChatArtifact(rec, req, chatID, "nope.txt", schema.GetChatArtifactParams{})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetChatArtifact_UnknownChat_404(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/nope/artifacts/doc.pdf", nil)
	rec := httptest.NewRecorder()
	h.GetChatArtifact(rec, req, "nope", "doc.pdf", schema.GetChatArtifactParams{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestListArtifactRevisions_NewestFirstWithLineage(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)

	saveTestArtifact(t, h, userID, chatID, "turn-1", "finding:abc123", "application/json", []byte(`{"v":1}`))
	saveTestArtifact(t, h, userID, chatID, "turn-2", "finding:abc123", "application/json", []byte(`{"v":2}`))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/finding:abc123/revisions", nil)
	rec := httptest.NewRecorder()
	h.ListArtifactRevisions(rec, req, chatID, "finding:abc123")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out schema.ArtifactRevisionList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("Data = %+v, want 2 revisions", out.Data)
	}
	if out.Data[0].Revision != 2 || out.Data[1].Revision != 1 {
		t.Errorf("revisions = %d, %d, want newest first (2, 1)", out.Data[0].Revision, out.Data[1].Revision)
	}
}

// TestListArtifactRevisions_UsesNameScopedQuery is the adversarial-review
// follow-up (#1094, then #1113): the endpoint must issue RevisionsForName's
// WHERE name = ? query, not the ListForSession fallback's full-chat scan -
// asserted on the raw SQL gorm renders, not QueryCount, since both paths
// issue exactly one SELECT (a bare count can't tell them apart; it only
// guards against N+1, not against an unscoped single scan).
func TestListArtifactRevisions_UsesNameScopedQuery(t *testing.T) {
	h := newTestHandler(t)
	h.store.EnableQueryRecording() // off by default in production; this test is the one real consumer
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)

	for i := 0; i < 20; i++ {
		saveTestArtifact(t, h, userID, chatID, "turn-1", fmt.Sprintf("finding:noise-%d", i), "application/json", []byte("{}"))
	}
	saveTestArtifact(t, h, userID, chatID, "turn-1", "finding:target", "application/json", []byte(`{"v":1}`))
	saveTestArtifact(t, h, userID, chatID, "turn-2", "finding:target", "application/json", []byte(`{"v":2}`))

	before := len(h.store.RecordedQuerySQL())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/finding:target/revisions", nil)
	rec := httptest.NewRecorder()
	h.ListArtifactRevisions(rec, req, chatID, "finding:target")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	seenScoped, seenUnscopedArtifactScan := false, false
	for _, sql := range h.store.RecordedQuerySQL()[before:] {
		if !strings.Contains(sql, "`artifacts`") { // sqlite renders identifiers backtick-quoted, not double-quoted
			continue // requireChat/sessionUser's own lookups - not the query under test
		}
		// "AND name = ?", not "name = " - the latter also matches "app_name = ?",
		// which both the scoped query AND the ListForSession fallback issue.
		if strings.Contains(sql, "AND name = ?") {
			seenScoped = true
			continue
		}
		if strings.Contains(sql, "session_id IN") {
			// ListForSession's full-chat scan (internal/store/artifact.go) - no
			// name filter, exactly the regression this test guards against.
			seenUnscopedArtifactScan = true
		}
	}
	if !seenScoped {
		t.Errorf("no name-scoped (WHERE name = ...) artifact query issued; recorded SQL: %v", h.store.RecordedQuerySQL()[before:])
	}
	if seenUnscopedArtifactScan {
		t.Errorf("revisions endpoint issued an unscoped full-chat artifact scan; recorded SQL: %v", h.store.RecordedQuerySQL()[before:])
	}
}

func TestListArtifactRevisions_UnknownArtifact_404(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/nope/revisions", nil)
	rec := httptest.NewRecorder()
	h.ListArtifactRevisions(rec, req, chatID, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDiffArtifactRevisions_TextUnifiedDiff(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)

	saveTestArtifact(t, h, userID, chatID, "turn-1", "code_review:pr:1", "application/json", []byte("{\"verdict\":\"comment\"}\n"))
	saveTestArtifact(t, h, userID, chatID, "turn-2", "code_review:pr:1", "application/json", []byte("{\"verdict\":\"approve\"}\n"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/code_review:pr:1/diff?from=1&to=2", nil)
	rec := httptest.NewRecorder()
	h.DiffArtifactRevisions(rec, req, chatID, "code_review:pr:1", schema.DiffArtifactRevisionsParams{From: 1, To: 2})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "-{\"verdict\":\"comment\"}") || !strings.Contains(body, "+{\"verdict\":\"approve\"}") {
		t.Errorf("diff = %q, want a unified diff between the two revisions", body)
	}
}

func TestDiffArtifactRevisions_BinaryBlob_415(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)

	saveTestArtifact(t, h, userID, chatID, "turn-1", "doc.pdf", "application/pdf", []byte("old"))
	saveTestArtifact(t, h, userID, chatID, "turn-2", "doc.pdf", "application/pdf", []byte("new"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/doc.pdf/diff?from=1&to=2", nil)
	rec := httptest.NewRecorder()
	h.DiffArtifactRevisions(rec, req, chatID, "doc.pdf", schema.DiffArtifactRevisionsParams{From: 1, To: 2})

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDiffArtifactRevisions_OversizeRevision_413(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)

	big := strings.Repeat("a", artifactref.InlineMaxBytes+1)
	saveTestArtifact(t, h, userID, chatID, "turn-1", "text:huge", "text/plain", []byte("small"))
	saveTestArtifact(t, h, userID, chatID, "turn-2", "text:huge", "text/plain", []byte(big))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/text:huge/diff?from=1&to=2", nil)
	rec := httptest.NewRecorder()
	h.DiffArtifactRevisions(rec, req, chatID, "text:huge", schema.DiffArtifactRevisionsParams{From: 1, To: 2})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDiffArtifactRevisions_UnknownRevision_404(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	userID := h.sessionUser(context.Background(), chatID)
	saveTestArtifact(t, h, userID, chatID, "turn-1", "finding:abc", "application/json", []byte(`{}`))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/finding:abc/diff?from=1&to=2", nil)
	rec := httptest.NewRecorder()
	h.DiffArtifactRevisions(rec, req, chatID, "finding:abc", schema.DiffArtifactRevisionsParams{From: 1, To: 2})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestGetChatArtifact_InlineAllowlist is the security-critical case: only
// the allowlisted image mime types render inline, and SVG - despite being
// "an image" colloquially - must NOT, since it can carry a <script>.
func TestGetChatArtifact_InlineAllowlist(t *testing.T) {
	cases := []struct {
		name string
		mime string
		want string
	}{
		{"png inline", "image/png", "inline"},
		{"jpeg inline", "image/jpeg", "inline"},
		{"gif inline", "image/gif", "inline"},
		{"webp inline", "image/webp", "inline"},
		{"svg forced download", "image/svg+xml", "attachment"},
		{"html forced download", "text/html", "attachment"},
		{"pdf forced download", "application/pdf", "attachment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHandler(t)
			chatID := mustCreateChat(t, h)
			userID := h.sessionUser(context.Background(), chatID)
			saveTestArtifact(t, h, userID, chatID, "turn-1", "f", c.mime, []byte("data"))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts/f", nil)
			rec := httptest.NewRecorder()
			h.GetChatArtifact(rec, req, chatID, "f", schema.GetChatArtifactParams{})

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			cd := rec.Header().Get("Content-Disposition")
			if got := cd[:len(c.want)]; got != c.want {
				t.Errorf("Content-Disposition = %q, want it to start with %q", cd, c.want)
			}
			if ct := rec.Header().Get("Content-Type"); ct != c.mime {
				t.Errorf("Content-Type = %q, want %q", ct, c.mime)
			}
		})
	}
}

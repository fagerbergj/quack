package remarkable

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
)

// fakeDispatchHost captures dispatched requests and can inject a per-call error.
type fakeDispatchHost struct {
	mu    sync.Mutex
	calls []sdk.DispatchRequest
	fn    func(req sdk.DispatchRequest) error
}

func (f *fakeDispatchHost) dispatch(ctx context.Context, req sdk.DispatchRequest) error {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return nil
}

func (f *fakeDispatchHost) dispatchedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Chat.LocalID)
	}
	return out
}

// newTestExtension wires an extension against a fresh fake rmfakecloud and a
// loaded (empty) state file - the state Start would normally load.
func newTestExtension(t *testing.T) (*extension, *fakeRMCloud, *fakeDispatchHost) {
	t.Helper()
	fc := newFakeRMCloud("user@example.com", "pw")
	t.Cleanup(fc.Close)
	fh := &fakeDispatchHost{}
	dir := t.TempDir()
	e := &extension{
		host: sdk.Host{
			Dispatch: fh.dispatch,
			Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			DataDir:  dir,
		},
		client:    newRMClient(fc.Server.URL, fc.email, fc.password),
		statePath: statePath(dir),
	}
	st, err := loadState(e.statePath)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	e.st = st
	return e, fc, fh
}

func testRouter(e *extension) chi.Router {
	r := chi.NewRouter()
	e.RegisterRoutes(r, chi.NewRouter())
	return r
}

var testMod = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func threeDocs() []fixtureDoc {
	return []fixtureDoc{
		{ID: "doc-c", Name: "Cortex", LastModified: testMod, PDF: []byte("%PDF-c")},
		{ID: "doc-a", Name: "Agenda", Folder: "Inbox", LastModified: testMod, PDF: []byte("%PDF-a")},
		{ID: "doc-b", Name: "Budget", Folder: "Inbox/2026", LastModified: testMod, PDF: []byte("%PDF-b")},
	}
}

func TestDocumentsPageListsDocumentsSortedWithCheckboxes(t *testing.T) {
	e, fc, _ := newTestExtension(t)
	fc.setDocs(threeDocs())

	rec := httptest.NewRecorder()
	testRouter(e).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/documents", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/remarkable/documents/ingest"`) {
		t.Errorf("form action missing:\n%s", body)
	}
	for _, id := range []string{"doc-a", "doc-b", "doc-c"} {
		if !strings.Contains(body, `<input type="checkbox" name="doc_id" value="`+id+`">`) {
			t.Errorf("checkbox for %s missing:\n%s", id, body)
		}
	}
	// Sorted by name: Agenda, Budget, Cortex - buildTree walks a map, so
	// without the sort this ordering is random.
	if a, b, c := strings.Index(body, "Agenda"), strings.Index(body, "Budget"), strings.Index(body, "Cortex"); !(a < b && b < c) {
		t.Errorf("rows not sorted by name (Agenda=%d Budget=%d Cortex=%d)", a, b, c)
	}
	if !strings.Contains(body, "Inbox/2026") {
		t.Errorf("folder column missing:\n%s", body)
	}
}

func TestDocumentsPageRendersErrorBannerWhenCloudDown(t *testing.T) {
	e, fc, _ := newTestExtension(t)
	fc.Close() // the nav entry must not 500 when rmfakecloud is unreachable

	rec := httptest.NewRecorder()
	testRouter(e).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/documents", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 with a banner", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Could not list documents") {
		t.Errorf("error banner missing:\n%s", rec.Body.String())
	}
}

func postIngest(t *testing.T, e *extension, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	testRouter(e).ServeHTTP(rec, req)
	return rec
}

func TestIngestDispatchesExactlyTheSelectedDocuments(t *testing.T) {
	e, fc, fh := newTestExtension(t)
	fc.setDocs(threeDocs())

	rec := postIngest(t, e, "/documents/ingest", url.Values{"doc_id": {"doc-a", "doc-c"}})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/remarkable/documents?queued=2" {
		t.Errorf("Location = %q", loc)
	}

	fh.mu.Lock()
	calls := fh.calls
	fh.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("dispatched %d docs (%v), want exactly 2", len(calls), fh.dispatchedIDs())
	}
	for _, c := range calls {
		if c.Run.Workflow != workflowDocumentIngest {
			t.Errorf("workflow = %q, want %q", c.Run.Workflow, workflowDocumentIngest)
		}
		if c.Chat.LocalID != "doc-a" && c.Chat.LocalID != "doc-c" {
			t.Errorf("dispatched unselected doc %q", c.Chat.LocalID)
		}
		if len(c.Ask.Attachments) != 1 || c.Ask.Attachments[0].Name != c.Chat.LocalID+".pdf" {
			t.Errorf("attachment = %+v, want %s.pdf", c.Ask.Attachments, c.Chat.LocalID)
		}
	}
	// Metadata comes from the live listing, not the form.
	for _, c := range calls {
		if c.Chat.LocalID == "doc-a" {
			if c.Chat.Title != "Agenda" || c.Chat.Origin.Labels["folder"][0].Value != "Inbox" {
				t.Errorf("doc-a metadata = %+v / %+v", c.Chat.Title, c.Chat.Origin.Labels)
			}
		}
	}

	if !e.st.Documents["doc-a"].InFlight || !e.st.Documents["doc-c"].InFlight {
		t.Errorf("selected docs not marked in flight: %+v", e.st.Documents)
	}
	if _, ok := e.st.Documents["doc-b"]; ok {
		t.Errorf("unselected doc-b got state: %+v", e.st.Documents["doc-b"])
	}
	if got := e.st.Documents["doc-a"].Attempts; got != 1 {
		t.Errorf("Attempts = %d, want 1", got)
	}

	reloaded, err := loadState(e.statePath)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if !reloaded.Documents["doc-a"].InFlight {
		t.Errorf("state not persisted: %+v", reloaded.Documents)
	}
}

func TestIngestIgnoresQueryStringSelections(t *testing.T) {
	e, fc, fh := newTestExtension(t)
	fc.setDocs(threeDocs())

	postIngest(t, e, "/documents/ingest?doc_id=doc-b", url.Values{"doc_id": {"doc-a"}})

	if got := fh.dispatchedIDs(); len(got) != 1 || got[0] != "doc-a" {
		t.Fatalf("dispatched %v, want only doc-a (query string must not select)", got)
	}
}

func TestIngestWithNoSelectionDispatchesNothing(t *testing.T) {
	e, fc, fh := newTestExtension(t)
	fc.setDocs(threeDocs())

	rec := postIngest(t, e, "/documents/ingest", url.Values{})

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != documentsPath {
		t.Errorf("code = %d, Location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := fh.dispatchedIDs(); len(got) != 0 {
		t.Errorf("dispatched %v, want none", got)
	}
}

func TestIngestFailedDownloadDoesNotAbortTheRest(t *testing.T) {
	e, fc, fh := newTestExtension(t)
	fc.setDocs(threeDocs())
	fc.failDownloads("doc-b")

	rec := postIngest(t, e, "/documents/ingest", url.Values{"doc_id": {"doc-b", "doc-a", "doc-c"}})

	got := fh.dispatchedIDs()
	if len(got) != 2 || got[0] != "doc-a" || got[1] != "doc-c" {
		t.Fatalf("dispatched %v, want doc-a and doc-c after doc-b failed", got)
	}
	if loc := rec.Header().Get("Location"); loc != "/remarkable/documents?failed=1&queued=2" {
		t.Errorf("Location = %q, want queued=2 failed=1", loc)
	}
	if _, ok := e.st.Documents["doc-b"]; ok {
		t.Errorf("failed doc-b should not be marked in flight: %+v", e.st.Documents["doc-b"])
	}
}

func TestIngestCapsSelectionsPerSubmit(t *testing.T) {
	e, fc, fh := newTestExtension(t)
	docs := threeDocs()
	ids := url.Values{}
	for i := 0; i <= maxPerSubmit; i++ {
		id := "doc-" + string(rune('m'+i))
		docs = append(docs, fixtureDoc{ID: id, Name: id, LastModified: testMod, PDF: []byte("x")})
		ids.Add("doc_id", id)
	}
	fc.setDocs(docs)

	rec := postIngest(t, e, "/documents/ingest", ids)

	if loc := rec.Header().Get("Location"); loc != documentsPath+"?limit=1" {
		t.Errorf("Location = %q, want the over-cap redirect", loc)
	}
	if got := fh.dispatchedIDs(); len(got) != 0 {
		t.Fatalf("dispatched %v, want none when over the cap", got)
	}

	// ...and the page explains why nothing ran.
	pageRec := httptest.NewRecorder()
	testRouter(e).ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, "/documents?limit=1", nil))
	if !strings.Contains(pageRec.Body.String(), "at most") {
		t.Errorf("cap notice missing:\n%s", pageRec.Body.String())
	}
}

func TestRunEndedClearsInFlightForNamespacedChatID(t *testing.T) {
	e, _, _ := newTestExtension(t)
	e.st.Documents["doc-a"] = docState{ID: "doc-a", Name: "Agenda", InFlight: true, Attempts: 1}

	e.RunEnded("ext:remarkable:doc-a", sdk.RunOutcome{Status: sdk.RunFailed, Answer: "boom"})

	got := e.st.Documents["doc-a"]
	if got.InFlight {
		t.Error("InFlight not cleared for the namespaced chat id the host actually sends")
	}
	if got.LastOutcome != "failed" || got.LastError != "boom" {
		t.Errorf("outcome = %+v", got)
	}
}

func TestRunEndedStillAcceptsBareDocumentID(t *testing.T) {
	e, _, _ := newTestExtension(t)
	e.st.Documents["doc-a"] = docState{ID: "doc-a", InFlight: true}

	e.RunEnded("doc-a", sdk.RunOutcome{Status: sdk.RunDone})

	if e.st.Documents["doc-a"].InFlight {
		t.Error("InFlight not cleared for a bare document id")
	}
}

func TestRunEndedIgnoresUnknownChatID(t *testing.T) {
	e, _, _ := newTestExtension(t)
	e.RunEnded("ext:remarkable:nope", sdk.RunOutcome{Status: sdk.RunDone})
	if len(e.st.Documents) != 0 {
		t.Errorf("unknown chat id created state: %+v", e.st.Documents)
	}
}

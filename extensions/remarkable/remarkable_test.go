package remarkable

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
)

func testHost(t *testing.T, fh *fakeDispatchHost) sdk.Host {
	t.Helper()
	return sdk.Host{
		Dispatch: fh.dispatch,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:  t.TempDir(),
	}
}

func TestFactoryRequiresBaseURL(t *testing.T) {
	fh := &fakeDispatchHost{}
	_, err := factory(testHost(t, fh), []byte("email: a@b.com\npassword: pw\n"))
	if err == nil {
		t.Fatal("expected an error for missing base_url, got nil")
	}
}

func TestFactoryRequiresCredentials(t *testing.T) {
	fh := &fakeDispatchHost{}
	_, err := factory(testHost(t, fh), []byte("base_url: http://example.com\n"))
	if err == nil {
		t.Fatal("expected an error for missing email/password, got nil")
	}
}

func TestFactoryRequiresDataDir(t *testing.T) {
	fh := &fakeDispatchHost{}
	host := testHost(t, fh)
	host.DataDir = ""
	_, err := factory(host, []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\n"))
	if err == nil {
		t.Fatal("expected an error for empty Host.DataDir, got nil")
	}
}

// Stale poll_interval keys in an existing quack.yaml must not break startup.
func TestFactoryParsesConfigAndIgnoresRetiredKeys(t *testing.T) {
	fh := &fakeDispatchHost{}
	raw := []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\npoll_interval: 5m\nmax_attempts: 7\n")
	extVal, err := factory(testHost(t, fh), raw)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ext := extVal.(*extension)
	if ext.client.baseURL != "http://example.com" {
		t.Errorf("baseURL = %q, want http://example.com", ext.client.baseURL)
	}
	if ext.statePath == "" {
		t.Error("statePath not set")
	}
}

func TestRegisteredInInit(t *testing.T) {
	factories := sdk.Registered()
	if _, ok := factories[extensionName]; !ok {
		t.Fatalf("sdk.Registered() missing %q - init() should have registered it", extensionName)
	}
}

func TestExtensionImplementsInterfaces(t *testing.T) {
	fh := &fakeDispatchHost{}
	extVal, err := factory(testHost(t, fh), []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ext := extVal.(*extension)

	if ext.Tools() != nil {
		t.Errorf("Tools() = %v, want nil (inbound-only extension)", ext.Tools())
	}
	if ui := ext.UI(); ui.Title == "" || ui.Href != documentsPath {
		t.Errorf("UI() = %+v, want the documents page as the nav entry", ui)
	}
}

func TestStartFailsLoudOnUnreachableCloud(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	fc.Close()
	fh := &fakeDispatchHost{}
	host := testHost(t, fh)
	e := &extension{host: host, client: newRMClient(fc.Server.URL, "user@example.com", "pw"), statePath: statePath(host.DataDir)}

	err := e.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded against a closed rmfakecloud, want a loud failure")
	}
	if !strings.Contains(err.Error(), "cannot reach rmfakecloud") {
		t.Errorf("err = %v, want it to name the unreachable cloud", err)
	}
}

func TestStartClearsStaleInFlight(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "pw")
	defer fc.Close()
	fh := &fakeDispatchHost{}
	host := testHost(t, fh)
	path := statePath(host.DataDir)

	// A previous process died mid-run: in_flight never got cleared.
	st := &state{Documents: map[string]docState{"doc-a": {ID: "doc-a", InFlight: true}}}
	if err := st.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	e := &extension{host: host, client: newRMClient(fc.Server.URL, fc.email, fc.password), statePath: path}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if e.st.Documents["doc-a"].InFlight {
		t.Error("Start left a stale in-flight document set")
	}
	reloaded, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if reloaded.Documents["doc-a"].InFlight {
		t.Error("cleared in-flight flag was not persisted")
	}
}

func TestStatusRouteReportsDispatchedDocuments(t *testing.T) {
	e, fc, _ := newTestExtension(t)
	fc.setDocs(threeDocs())

	req := httptest.NewRequest(http.MethodPost, "/documents/ingest", strings.NewReader("doc_id=doc-a"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r := chi.NewRouter()
	e.RegisterRoutes(r, chi.NewRouter())
	r.ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Extension != extensionName {
		t.Errorf("Extension = %q, want %q", got.Extension, extensionName)
	}
	if len(got.Documents) != 1 || got.Documents[0].ID != "doc-a" {
		t.Fatalf("Documents = %+v, want one entry doc-a", got.Documents)
	}
	if got.Documents[0].Folder != "Inbox" || !got.Documents[0].InFlight || got.Documents[0].Attempts != 1 {
		t.Errorf("Documents[0] = %+v", got.Documents[0])
	}
}

// TestStatusRouteBeforeAnyIngestDoesNotPanic exercises /status with no state
// loaded at all (Start never ran) - status_test.go covers rendered content.
func TestStatusRouteBeforeAnyIngestDoesNotPanic(t *testing.T) {
	fh := &fakeDispatchHost{}
	extVal, err := factory(testHost(t, fh), []byte("base_url: http://example.com\nemail: a@b.com\npassword: pw\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ext := extVal.(*extension)
	r := chi.NewRouter()
	ext.RegisterRoutes(r, chi.NewRouter())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// Downloads must outlive the request: a proxy timeout during a batch cannot
// be allowed to silently drop the documents that hadn't started yet.
func TestIngestSurvivesRequestCancellation(t *testing.T) {
	e, fc, fh := newTestExtension(t)
	fc.setDocs(threeDocs())

	ctx, cancel := context.WithCancel(context.Background())
	fh.fn = func(sdk.DispatchRequest) error {
		cancel() // client hangs up after the first dispatch
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/documents/ingest", strings.NewReader("doc_id=doc-a&doc_id=doc-c")).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	testRouter(e).ServeHTTP(rec, req)

	if got := fh.dispatchedIDs(); len(got) != 2 {
		t.Fatalf("dispatched %v, want both docs despite the cancelled request", got)
	}
}

func TestFactoryRejectsMalformedYAML(t *testing.T) {
	fh := &fakeDispatchHost{}
	if _, err := factory(testHost(t, fh), []byte("base_url: [")); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("err = %v, want a parse error", err)
	}
}

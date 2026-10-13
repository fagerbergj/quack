package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/extensions/sdk/extproc"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
)

// fakeRMCloud serves rmfakecloud's login, document tree and PDF export; doc-2's export fails.
func fakeRMCloud(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/api/login", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "tok") })
	mux.HandleFunc("GET /ui/api/documents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Entries":[{"id":"f1","name":"Work","isFolder":true,"lastModified":"2026-10-01T10:00:00Z",`+
			`"children":[{"id":"doc-1","name":"Meeting","lastModified":"2026-10-02T10:00:00Z"}]},`+
			`{"id":"doc-2","name":"Sketch","lastModified":"2026-10-03T10:00:00Z"}]}`)
	})
	mux.HandleFunc("GET /ui/api/documents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "doc-2" {
			http.Error(w, "export failed", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "%PDF-"+r.PathValue("id"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// remarkableFlow is everything a user and quack observe of one browse, ingest and finished run.
type remarkableFlow struct {
	documents, ingestLocation, status, statusJSON string
	chatTitle, chatOrigin                         string
	turns                                         int
}

func getBody(t *testing.T, client *http.Client, u string) string {
	t.Helper()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// runRemarkable boots extensions.remarkable from block on a real store and stub-model orchestrator, browses,
// ingests three documents (one exports, one fails, one is gone) and waits for RunEnded to settle the status.
// child reports whether quack ran it as a child process.
func runRemarkable(t *testing.T, block string) (f remarkableFlow, child bool) {
	t.Helper()
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]
	shapes := shapesRefOf([]workflowcatalog.Shape{{Name: "document-ingest", Trigger: "t", DAGShape: "s"}})
	cfg := noopModulesConfig(t, t.TempDir(), block)
	exts, err := buildSDKExtensions(cfg, st, hub, runlog.NewEventLog(st), &orchRef, artifacts, jail, &judgeModelRef, nil, nil, nil, shapes, nil)
	if err != nil || len(exts) != 1 {
		t.Fatalf("buildSDKExtensions = %d exts, %v", len(exts), err)
	}
	t.Cleanup(func() { closeSDKExtensions(exts) })
	if err := startSDKExtensions(context.Background(), exts); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(server.Options{REST: &rest.Handler{}, SDKExtensions: sdkExtensionMounts(exts)}))
	t.Cleanup(ts.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	_, child = exts[0].ext.(*extproc.Process)
	f.documents = getBody(t, client, ts.URL+"/remarkable/documents")
	resp, err := client.PostForm(ts.URL+"/remarkable/documents/ingest", url.Values{"doc_id": {"doc-1", "doc-2", "gone"}})
	if err != nil {
		t.Fatalf("POST ingest: %v", err)
	}
	_ = resp.Body.Close()
	f.ingestLocation = resp.Header.Get("Location")
	waitUntil(t, 10*time.Second, func() bool {
		return strings.Contains(getBody(t, client, ts.URL+"/remarkable/status.json"), `"in_flight":false,"last_outcome":"done"`)
	})
	f.statusJSON = getBody(t, client, ts.URL+"/remarkable/status.json")
	f.status = getBody(t, client, ts.URL+"/remarkable/status")

	ctx := context.Background()
	c, err := st.GetChat(ctx, "ext:remarkable:doc-1")
	if err != nil || c == nil {
		t.Fatalf("GetChat = %v, %v", c, err)
	}
	f.chatTitle, f.chatOrigin = c.Title, c.Origin
	turns, _ := st.ListTurns(ctx, c.ID)
	f.turns = len(turns)
	return f, child
}

// TestRemarkableOutOfProcessMatchesInProcess: with exec set, remarkable's Dispatch reaches quack and its
// RunEnded reaches the child, so browse, ingest and the settled status match the compiled-in extension.
func TestRemarkableOutOfProcessMatchesInProcess(t *testing.T) {
	bin := buildExtChild(t, "remarkable")
	block := "remarkable:\n  base_url: " + fakeRMCloud(t) + "\n  email: a@b.c\n  password: pw\n"

	want, _ := runRemarkable(t, block)
	got, child := runRemarkable(t, block+"  exec: "+bin+"\n")
	if !child {
		t.Fatal("exec did not run remarkable as a child process")
	}

	if want.ingestLocation != "/remarkable/documents?failed=2&queued=1" || want.turns != 1 || want.chatTitle != "Meeting" ||
		!strings.Contains(want.chatOrigin, `"Labels":{"folder"`) {
		t.Errorf("in-process flow = %+v, want doc-1 queued as one Meeting turn with its folder label", want)
	}
	if got != want {
		t.Errorf("out of process:\n%+v\nin process:\n%+v", got, want)
	}
}

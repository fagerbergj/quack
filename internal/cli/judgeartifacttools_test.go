package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/schema"
)

func TestRESTArtifactToolsBuilds(t *testing.T) {
	c := &Client{BaseURL: "http://example.invalid", HTTP: &http.Client{}}
	tools, err := RESTArtifactTools(c, "chat-1")
	if err != nil {
		t.Fatalf("RESTArtifactTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name() != "list_artifacts" || tools[1].Name() != "read_artifact" {
		t.Fatalf("tools = %+v, want [list_artifacts, read_artifact]", tools)
	}
}

func TestStubArtifactToolsBuilds(t *testing.T) {
	tools, err := StubArtifactTools()
	if err != nil {
		t.Fatalf("StubArtifactTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name() != "list_artifacts" || tools[1].Name() != "read_artifact" {
		t.Fatalf("tools = %+v, want [list_artifacts, read_artifact]", tools)
	}
}

func TestFormatArtifactSummaries(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	int64Ptr := func(i int64) *int64 { return &i }
	items := []schema.ArtifactSummary{
		{Name: "a", Kind: strPtr("text"), LatestRevision: int64Ptr(3)},
		{Name: "b", Kind: strPtr("doc"), LatestRevision: int64Ptr(1)},
	}
	if got := formatArtifactSummaries(nil, ""); got != "(no artifacts)" {
		t.Errorf("empty list = %q, want the no-artifacts placeholder", got)
	}
	all := formatArtifactSummaries(items, "")
	if !strings.Contains(all, "a\trevision=3\tkind=text") || !strings.Contains(all, "b\trevision=1\tkind=doc") {
		t.Errorf("formatArtifactSummaries(all) = %q, want both rows", all)
	}
	filtered := formatArtifactSummaries(items, "doc")
	if strings.Contains(filtered, "kind=text") || !strings.Contains(filtered, "kind=doc") {
		t.Errorf("formatArtifactSummaries(kind=doc) = %q, want only the doc row", filtered)
	}
}

type restToolCtx struct{ adkagent.StrictContextMock }

func (restToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// TestRESTReadArtifactResolvesPartialID: replay's read_artifact resolves a partial id against the
// chat's listed names, names an ambiguous one's candidates, and surfaces a failed listing.
func TestRESTReadArtifactResolvesPartialID(t *testing.T) {
	var listFails atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/chats/chat-1/artifacts" && listFails.Load():
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/api/v1/chats/chat-1/artifacts":
			_, _ = w.Write([]byte(`{"data":[{"name":"trends:doc:33db"},{"name":"web_page:323f"},{"name":"web_page:9a0b"}]}`))
		case r.URL.Path == "/api/v1/chats/chat-1/artifacts/trends:doc:33db":
			_, _ = w.Write([]byte("doc body"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	tools, err := RESTArtifactTools(&Client{BaseURL: srv.URL, HTTP: srv.Client()}, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	read := tools[1].(interface {
		Run(adkagent.Context, any) (map[string]any, error)
	})
	ctx := &restToolCtx{StrictContextMock: adkagent.NewStrictContextMock(t.Context())}
	if out, err := read.Run(ctx, map[string]any{"id": "33db"}); err != nil || out["result"] != recordstore.ResolvedIDPrefix+"trends:doc:33db\n\ndoc body" {
		t.Errorf("read(33db) = %v, %v; want the doc under its full id", out["result"], err)
	}
	if _, err := read.Run(ctx, map[string]any{"id": "web_page"}); err == nil || !strings.Contains(err.Error(), "web_page:9a0b") {
		t.Errorf("read(web_page) err = %v, want an ambiguity naming both pages", err)
	}
	listFails.Store(true)
	if _, err := read.Run(ctx, map[string]any{"id": "33db"}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("read with a failing listing: err = %v, want the listing's failure", err)
	}
}

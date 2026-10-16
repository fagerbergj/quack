package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/toolconfirmation"

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

// TestRESTReadArtifactMissListsIDs: replay's read_artifact names the listed ids when an id misses.
func TestRESTReadArtifactMissListsIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/chats/chat-1/artifacts":
			_, _ = w.Write([]byte(`{"data":[{"name":"trends:doc:33db"},{"name":"web_page:323f"}]}`))
		case "/api/v1/chats/chat-1/artifacts/trends:doc:33db":
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
	if out, err := read.Run(ctx, map[string]any{"id": "trends:doc:33db"}); err != nil || out["result"] != "doc body" {
		t.Errorf("read(full id) = %v, %v; want the doc body", out["result"], err)
	}
	if _, err := read.Run(ctx, map[string]any{"id": "33db"}); err == nil || !strings.Contains(err.Error(), "trends:doc:33db") || !strings.Contains(err.Error(), "web_page:323f") {
		t.Errorf("read(33db) err = %v, want not found listing both ids", err)
	}
}

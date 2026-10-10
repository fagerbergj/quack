package cli

import (
	"net/http"
	"strings"
	"testing"

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

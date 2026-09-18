package vetting

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// TestLatestArtifactContent_NoArtifactsService: recordClient's own nil guard -
// a chat with no artifact.Service configured has nothing to inline, ever.
func TestLatestArtifactContent_NoArtifactsService(t *testing.T) {
	content, _, ok := LatestArtifactContent(context.Background(), Config{ChatID: "chat1"}, "dep")
	if ok || content != "" {
		t.Errorf("LatestArtifactContent = (%q, ok=%v), want (\"\", false) with no Artifacts service", content, ok)
	}
}

// TestLatestArtifactContent_TypedDocument covers deliveryTarget's own kind
// (cfg.Artifact = "document"), the same identity a document-kind node's saved
// revision uses.
func TestLatestArtifactContent_TypedDocument(t *testing.T) {
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	want := strings.Repeat("the full document body. ", 10)
	if _, _, err := c.SaveBlob(context.Background(), "document", []byte(want), "text/markdown", "doc:chat1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1", Artifact: "document"}

	got, rev, ok := LatestArtifactContent(context.Background(), cfg, "dep")
	if !ok || got != want || rev != 1 {
		t.Errorf("LatestArtifactContent = (%q, rev=%d, ok=%v), want (%q, rev=1, ok=true)", got, rev, ok, want)
	}
}

// TestLatestArtifactContent_GenericTextFallback covers the "text:<nodeID>"
// identity every gate writes (reviewrecord.go's saveTextRound) for a node with
// no configured Artifact kind and no IsReviewer selector.
func TestLatestArtifactContent_GenericTextFallback(t *testing.T) {
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	want := strings.Repeat("the episodic round answer. ", 10)
	if _, _, err := c.SaveBlob(context.Background(), "text", []byte(want), "text/markdown", "dep-node", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed text record: %v", err)
	}
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1"}

	got, _, ok := LatestArtifactContent(context.Background(), cfg, "dep-node")
	if !ok || got != want {
		t.Errorf("LatestArtifactContent = (%q, ok=%v), want (%q, ok=true)", got, ok, want)
	}
}

// TestLatestArtifactContent_NothingSaved: an Artifacts service is configured,
// but nothing was ever saved under either identity - no artifact to inline.
func TestLatestArtifactContent_NothingSaved(t *testing.T) {
	svc := artifact.InMemoryService()
	cfg := Config{Artifacts: svc, User: "u1", ChatID: "chat1"}

	content, _, ok := LatestArtifactContent(context.Background(), cfg, "dep-node")
	if ok || content != "" {
		t.Errorf("LatestArtifactContent = (%q, ok=%v), want (\"\", false) with nothing saved", content, ok)
	}
}

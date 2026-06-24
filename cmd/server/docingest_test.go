package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

// TestDocIngestResourceValid checks the canonical doc-ingest DAG resource parses
// and validates against the doc-ingest agent roster once its placeholders are
// filled (what the ingestion endpoint will do).
func TestDocIngestResourceValid(t *testing.T) {
	raw, err := os.ReadFile("../../skills/plan-work/resources/doc-ingest.json")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.NewReplacer("{{extract_agent}}", "media-reader", "{{content_hash}}", "abc123").Replace(string(raw))
	var doc struct {
		Nodes []dag.RawNode `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatalf("doc-ingest.json invalid: %v", err)
	}
	p := dag.NewPlanner([]dag.AgentInfo{
		{Name: "media-reader"}, {Name: "general-purpose"}, {Name: "classifier"}, {Name: "document-writer"},
	})
	if _, err := p.Build(doc.Nodes, nil, "ingest", nil); err != nil {
		t.Fatalf("doc-ingest DAG failed validation: %v", err)
	}
}

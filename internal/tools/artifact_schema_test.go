// artifact_schema_test.go: the native write_artifact/edit_artifact tools
// refuse a write that fails its kind's registered schema (extsdk.ArtifactSchemas),
// mirroring internal/acp/artifact_schema_test.go's MCP-path coverage.
package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/recordstore"
)

func nameSchemaRegistry(t *testing.T, kind string) *artifactschema.Registry {
	t.Helper()
	reg, err := artifactschema.Build(map[string]map[string]json.RawMessage{
		"fake-ext": {kind: json.RawMessage(`{"type":"object","required":["name"]}`)},
	})
	if err != nil {
		t.Fatalf("artifactschema.Build: %v", err)
	}
	return reg
}

func TestWriteArtifact_SchemaValid_Succeeds(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a").WithSchemas(nameSchemaRegistry(t, "document"))
	tl, err := NewWriteArtifactTool(rc, "n1", &RoundCoords{}, "hint")
	if err != nil {
		t.Fatal(err)
	}
	rt := tl.(runnableTool)
	out, err := rt.Run(newArtifactsToolCtx(), map[string]any{"kind": "document", "mime": "application/json", "bytes": `{"name":"trade idea"}`})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	result, _ := out["result"].(string)
	if !strings.Contains(result, "ok:") {
		t.Fatalf("result = %q, want ok", result)
	}
}

// TestWriteArtifact_SchemaViolation_RefusesAndCarriesSchema pins the exact text
// a model sees on a schema-violating write_artifact call: the write must not
// land, and the message must name the kind, list the violation, and tell the
// model to retry.
func TestWriteArtifact_SchemaViolation_RefusesAndCarriesSchema(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a").WithSchemas(nameSchemaRegistry(t, "document"))
	tl, err := NewWriteArtifactTool(rc, "n1", &RoundCoords{}, "hint")
	if err != nil {
		t.Fatal(err)
	}
	rt := tl.(runnableTool)
	_, err = rt.Run(newArtifactsToolCtx(), map[string]any{"kind": "document", "mime": "application/json", "bytes": `{"other":1}`})
	if err == nil {
		t.Fatal("write_artifact violating its kind's schema should be refused")
	}
	got := err.Error()
	if !strings.Contains(got, "required") {
		t.Errorf("error = %q, want it to mention the missing required property", got)
	}
	if !strings.Contains(got, `"type":"object"`) {
		t.Errorf("error = %q, want it to carry the kind's schema so one retry can fix everything", got)
	}
	items, err := rc.List(context.Background(), "document")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("text artifacts = %d, want 0 (a schema-violating write must not land)", len(items))
	}
}

func TestWriteArtifact_InvalidJSON_Refuses(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a").WithSchemas(nameSchemaRegistry(t, "document"))
	tl, err := NewWriteArtifactTool(rc, "n1", &RoundCoords{}, "hint")
	if err != nil {
		t.Fatal(err)
	}
	rt := tl.(runnableTool)
	_, err = rt.Run(newArtifactsToolCtx(), map[string]any{"kind": "document", "mime": "application/json", "bytes": "not json"})
	if err == nil {
		t.Fatal("write_artifact with non-JSON content against a JSON-schema'd kind should be refused")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error = %q, want it to say the content is not valid JSON", err.Error())
	}
}

func TestWriteArtifact_UnregisteredKindUnaffected(t *testing.T) {
	// Registry only knows about "document" - "bytes" has no declared schema, so
	// arbitrary content must still write exactly as before this feature.
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a").WithSchemas(nameSchemaRegistry(t, "document"))
	tl, err := NewWriteArtifactTool(rc, "n1", &RoundCoords{}, "hint")
	if err != nil {
		t.Fatal(err)
	}
	rt := tl.(runnableTool)
	out, err := rt.Run(newArtifactsToolCtx(), map[string]any{"kind": "bytes", "mime": "text/plain", "bytes": "plain prose, not json"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result, _ := out["result"].(string); !strings.Contains(result, "ok:") {
		t.Fatalf("result = %q, want ok", result)
	}
}

func TestEditArtifact_SchemaViolation_RefusesAndPriorRevisionIntact(t *testing.T) {
	rc := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat-a").WithSchemas(nameSchemaRegistry(t, "document"))
	id, rev, err := rc.SaveBlob(context.Background(), "document", []byte(`{"name":"x"}`), "application/json", "doc-1", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	tl, err := NewEditArtifactTool(rc, "n1", &RoundCoords{})
	if err != nil {
		t.Fatal(err)
	}
	rt := tl.(runnableTool)
	_, err = rt.Run(newArtifactsToolCtx(), map[string]any{
		"id": id, "base_revision": rev,
		"edits": []editArtifactEdit{{Old: `"name":"x"`, New: `"other":"y"`}},
	})
	if err == nil {
		t.Fatal("edit_artifact producing schema-invalid content should be refused")
	}
	if !strings.Contains(err.Error(), `kind "document" failed its schema`) {
		t.Errorf("error = %q, want it to name the kind and the schema failure", err.Error())
	}
	raw, _, ok, gerr := rc.Latest(context.Background(), id)
	if gerr != nil || !ok || string(raw) != `{"name":"x"}` {
		t.Fatalf("Latest after refused edit: raw=%q ok=%v err=%v, want the prior revision intact", raw, ok, gerr)
	}
}

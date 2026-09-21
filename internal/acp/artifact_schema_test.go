// artifact_schema_test.go: the loopback MCP write_artifact/edit_artifact
// tools refuse a write that fails its kind's registered schema
// (extsdk.ArtifactSchemas), mirroring internal/tools/artifact_schema_test.go's
// native-path coverage - a model must see the same story on either surface.
package acp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

func nameSchemaRegistryMCP(t *testing.T, kind string) *artifactschema.Registry {
	t.Helper()
	reg, err := artifactschema.Build(map[string]map[string]json.RawMessage{
		"fake-ext": {kind: json.RawMessage(`{"type":"object","required":["name"]}`)},
	})
	if err != nil {
		t.Fatalf("artifactschema.Build: %v", err)
	}
	return reg
}

func TestWriteArtifactMCP_SchemaValid_Succeeds(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{
		Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1",
		Schemas: nameSchemaRegistryMCP(t, "text"),
	})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_artifact", Arguments: map[string]any{
		"kind": "text", "mime": "application/json", "bytes": `{"name":"trade idea"}`,
	}})
	if err != nil {
		t.Fatalf("CallTool write_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("write_artifact returned an error: %s", toolResultText(t, res))
	}
}

// TestWriteArtifactMCP_SchemaViolation_RefusesWithExactText pins the exact
// text a model sees on this surface - it must read identically to the native
// tool path (internal/tools's TestWriteArtifact_SchemaViolation_RefusesWithExactText).
func TestWriteArtifactMCP_SchemaViolation_RefusesWithExactText(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{
		Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1",
		Schemas: nameSchemaRegistryMCP(t, "text"),
	})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_artifact", Arguments: map[string]any{
		"kind": "text", "mime": "application/json", "bytes": `{"other":1}`,
	}})
	if err != nil {
		t.Fatalf("CallTool write_artifact: %v", err)
	}
	if !res.IsError {
		t.Fatalf("write_artifact violating its kind's schema should be refused, got: %s", toolResultText(t, res))
	}
	got := toolResultText(t, res)
	wantPrefix := `artifact not written: kind "text" failed its schema:`
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("result = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "required") {
		t.Errorf("result = %q, want it to mention the missing required property", got)
	}
	if !strings.HasSuffix(got, "Fix these and call the tool again.") {
		t.Errorf("result = %q, want it to end telling the model to retry", got)
	}

	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	items, err := rc.List(ctx, "text")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("text artifacts = %d, want 0 (a schema-violating write must not land)", len(items))
	}
}

func TestWriteArtifactMCP_InvalidJSON_Refuses(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{
		Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1",
		Schemas: nameSchemaRegistryMCP(t, "text"),
	})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_artifact", Arguments: map[string]any{
		"kind": "text", "mime": "application/json", "bytes": "not json",
	}})
	if err != nil {
		t.Fatalf("CallTool write_artifact: %v", err)
	}
	if !res.IsError {
		t.Fatalf("write_artifact with non-JSON content against a JSON-schema'd kind should be refused, got: %s", toolResultText(t, res))
	}
	if !strings.Contains(toolResultText(t, res), "not valid JSON") {
		t.Errorf("result = %q, want it to say the content is not valid JSON", toolResultText(t, res))
	}
}

func TestWriteArtifactMCP_UnregisteredKindUnaffected(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	// Registry only knows about "text" - "bytes" has no declared schema.
	vetting.RegisterMemSession(secret, vetting.MemSession{
		Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1",
		Schemas: nameSchemaRegistryMCP(t, "text"),
	})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "write_artifact", Arguments: map[string]any{
		"kind": "bytes", "mime": "text/plain", "bytes": "plain prose, not json",
	}})
	if err != nil {
		t.Fatalf("CallTool write_artifact: %v", err)
	}
	if res.IsError {
		t.Fatalf("write_artifact on an unschema'd kind should be unaffected, got: %s", toolResultText(t, res))
	}
}

func TestEditArtifactMCP_SchemaViolation_RefusesAndPriorRevisionIntact(t *testing.T) {
	ctx := context.Background()
	secret := mustMemSecret(t)
	svc := artifact.InMemoryService()
	vetting.RegisterMemSession(secret, vetting.MemSession{
		Artifacts: svc, AppName: "quack", UserID: "u1", ChatID: "chat-a", NodeID: "n1",
		Schemas: nameSchemaRegistryMCP(t, "text"),
	})
	defer vetting.UnregisterMemSession(secret)

	rc := recordstore.New(svc, "quack", "u1", "chat-a")
	id, rev, err := rc.SaveBlob(ctx, "text", []byte(`{"name":"x"}`), "application/json", "", recordstore.Lineage{NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "edit_artifact", Arguments: map[string]any{
		"id": id, "base_revision": rev,
		"edits": []map[string]any{{"old": `"name":"x"`, "new": `"other":"y"`}},
	}})
	if err != nil {
		t.Fatalf("CallTool edit_artifact: %v", err)
	}
	if !res.IsError {
		t.Fatalf("edit_artifact producing schema-invalid content should be refused, got: %s", toolResultText(t, res))
	}
	if !strings.Contains(toolResultText(t, res), `kind "text" failed its schema`) {
		t.Errorf("result = %q, want it to name the kind and the schema failure", toolResultText(t, res))
	}

	raw, _, ok, gerr := rc.Latest(ctx, id)
	if gerr != nil || !ok || string(raw) != `{"name":"x"}` {
		t.Fatalf("Latest after refused edit: raw=%q ok=%v err=%v, want the prior revision intact", raw, ok, gerr)
	}
}

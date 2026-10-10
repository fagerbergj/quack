// buildGateNodes must stamp cfg.Schemas unconditionally so an ungated agent's
// writes are still schema-checked.
package dag_test

import (
	"context"
	"encoding/json"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

func ungatedNameRequiredSchema(t *testing.T, kind string) *artifactschema.Registry {
	t.Helper()
	reg, err := artifactschema.Build(map[string]map[string]json.RawMessage{
		"fake-ext": {kind: json.RawMessage(`{"type":"object","required":["name"]}`)},
	})
	if err != nil {
		t.Fatalf("artifactschema.Build: %v", err)
	}
	return reg
}

// A zero vetting.Config (gates disabled) still enforces the node's
// "document" schema: prose falls back to text:<node>, never lands under the document id.
func TestBuildGateNodes_SchemasArmedForUngatedAgent(t *testing.T) {
	svc := artifact.InMemoryService()
	worker, err := llmagent.New(llmagent.Config{
		Name: "w", Model: textLLM("plain prose, not json at all"), Description: "w", Instruction: "ROLE:w Just answer.",
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	cfgFor := func(context.Context, string) vetting.Config { return vetting.Config{} } // the "ungated" shape
	ex := dag.NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": worker}, nil, nil, cfgFor, nil)
	ex.SetArtifacts(svc)
	ex.SetSchemas(ungatedNameRequiredSchema(t, "document"))

	const chatID = "ungated-chat"
	plan := dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{
		{ID: "n1", AgentName: "w", Task: "write it", Artifact: "document"},
	}}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", chatID, content,
		func(stream.SSEEvent, error) bool { return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	rc := recordstore.New(svc, artifactref.AppName, "u", chatID)
	docID, err := recordstore.IdentityFor("document", nil, vetting.DocumentHint(chatID))
	if err != nil {
		t.Fatal(err)
	}
	if raw, _, ok, lerr := rc.Latest(context.Background(), docID); lerr == nil && ok {
		t.Fatalf("document artifact stored despite failing its registered schema: %q", raw)
	}
}

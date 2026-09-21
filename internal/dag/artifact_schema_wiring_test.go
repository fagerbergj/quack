// artifact_schema_wiring_test.go: buildGateNodes must stamp cfg.Schemas
// unconditionally, in the same lines as cfg.Artifacts/User/Ledger, so an
// agent with no per-agent gate config (gated: false, or gates disabled
// altogether - cfgFor returning the zero vetting.Config either way) still
// gets its writes checked (B2).
package dag_test

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// proseStub answers with plain prose (no tool call, no JSON) on every call -
// the gate's own fallback save (saveDocumentRound) is what tries to store it
// under the node's declared artifact kind.
type proseStub struct{}

func (proseStub) Name() string { return "proseStub" }

func (proseStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "plain prose, not json at all"}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

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

// TestBuildGateNodes_SchemasArmedForUngatedAgent: cfgFor returns the ZERO
// vetting.Config (no Threshold/JudgeRounds/DeterministicRounds - exactly what
// an agent with gated:false, or gates disabled entirely, gets from
// resolveGateCfg/gateCfgs.boot). With SetSchemas armed on the Executor, the
// node's declared "document" kind still has its registered schema enforced:
// the gate's fallback save of plain prose must be refused and fall back to
// text:<node>, never landing under the document id as-is.
func TestBuildGateNodes_SchemasArmedForUngatedAgent(t *testing.T) {
	svc := artifact.InMemoryService()
	worker, err := llmagent.New(llmagent.Config{
		Name: "w", Model: proseStub{}, Description: "w", Instruction: "ROLE:w Just answer.",
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

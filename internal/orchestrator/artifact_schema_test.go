// artifact_schema_test.go: the orchestrator's own write_artifact tool must be
// checked against a registered extension schema exactly like a gated node's
// write_artifact - the orchestrator runs in every chat, so a bypass here
// reproduces the prod-shaped bad artifact this feature exists to stop (B1).
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

func nameRequiredOrchSchema(t *testing.T, kind string) *artifactschema.Registry {
	t.Helper()
	reg, err := artifactschema.Build(map[string]map[string]json.RawMessage{
		"fake-ext": {kind: json.RawMessage(`{"type":"object","required":["name"]}`)},
	})
	if err != nil {
		t.Fatalf("artifactschema.Build: %v", err)
	}
	return reg
}

// writeArtifactStub: first call requests write_artifact with the given
// kind/bytes, second call echoes whatever the tool result text was.
type writeArtifactStub struct {
	mu    sync.Mutex
	calls int
	kind  string
	bytes string
}

func (*writeArtifactStub) Name() string { return "writeArtifactStub" }

func (s *writeArtifactStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		if n == 1 {
			yield(&model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{Name: "write_artifact", Args: map[string]any{
						"kind": s.kind, "mime": "application/json", "bytes": s.bytes,
					}},
				}}},
				FinishReason: genai.FinishReasonStop, TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "tool said: " + lastRequestText(req)}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

// TestOrchestratorRun_WriteArtifact_SchemaViolationRefused: with SetSchemas
// armed, a write_artifact call whose content fails the kind's registered
// schema must be refused and nothing stored - reproduces the B1 probe
// (orchestrator's own tools bypassing WithSchemas) as a full Run.
func TestOrchestratorRun_WriteArtifact_SchemaViolationRefused(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	const userID, chatID = "u1", "c1"

	sessions := session.InMemoryService()
	stub := &writeArtifactStub{kind: "document", bytes: `{"other":1}`}
	o := New(sessions, stub, "you are the orchestrator", dag.NewPlanner(nil, nil, nil), dag.NewExecutor(sessions, nil, nil, nil, nil, nil), nil, nil, nil)
	o.SetArtifacts(svc)
	o.SetSchemas(nameRequiredOrchSchema(t, "document"))

	var texts []string
	for ev, err := range o.Run(ctx, userID, chatID, SourceApp, "write the artifact", nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		switch d := ev.Data.(type) {
		case stream.AgentTokenData:
			texts = append(texts, d.Text)
		case stream.AgentToolResultData:
			texts = append(texts, fmt.Sprint(d.Result))
		}
	}
	joined := strings.Join(texts, " ")
	if !strings.Contains(joined, `kind "document" failed its schema`) {
		t.Fatalf("orchestrator run output = %q, want the write refused for its schema violation", joined)
	}

	resp, err := svc.List(ctx, &artifact.ListRequest{AppName: AppName, UserID: userID, SessionID: chatID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, name := range resp.FileNames {
		if strings.HasPrefix(name, "text:") {
			t.Fatalf("artifact %q was stored despite the schema violation", name)
		}
	}
}

package orchestrator

import (
	"context"
	"iter"
	"sync"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// renderUIStub calls render_ui once with a one-component surface, then answers.
type renderUIStub struct {
	mu      sync.Mutex
	calls   int
	offered bool // render_ui was in the first request's tool map
}

func (*renderUIStub) Name() string { return "renderUIStub" }

func (s *renderUIStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		if s.calls == 1 {
			_, s.offered = req.Tools["render_ui"]
		}
		n := s.calls
		s.mu.Unlock()
		part := &genai.Part{Text: "done"}
		if n == 1 {
			part = &genai.Part{FunctionCall: &genai.FunctionCall{Name: "render_ui", Args: map[string]any{
				"surface_id": "s1", "components": []any{map[string]any{"id": "root", "component": "Text", "text": "hi"}},
			}}}
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// TestOrchestratorRun_RenderUI: an enabled render_ui saves the surface and its
// artifact_revision event reaches the orchestrator's own SSE stream.
func TestOrchestratorRun_RenderUI(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	sessions := session.InMemoryService()
	stub := &renderUIStub{}
	o := New(sessions, stub, func(context.Context) string { return "you are the orchestrator" }, dag.NewPlanner(nil, nil, nil), dag.NewExecutor(sessions, nil, nil, nil, nil, nil), nil, nil, nil)
	o.SetArtifacts(svc)
	o.SetRenderUI(true)

	var rev *stream.ArtifactRevisionData
	for ev, err := range o.Run(ctx, "u1", "c1", SourceApp, "show me", nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if d, ok := ev.Data.(stream.ArtifactRevisionData); ok {
			rev = &d
		}
	}
	if !stub.offered || rev == nil || rev.ID != "a2ui_surface:s1" || rev.Revision != 1 || rev.NodeID != orchestratorName {
		t.Fatalf("artifact_revision = %+v", rev)
	}
	if _, err := svc.Load(ctx, &artifact.LoadRequest{AppName: AppName, UserID: "u1", SessionID: "c1", FileName: "a2ui_surface:s1"}); err != nil {
		t.Fatalf("surface not stored: %v", err)
	}
}

func TestOrchestratorRun_RenderUIDisabled(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &renderUIStub{}
	o := New(sessions, stub, func(context.Context) string { return "you are the orchestrator" }, dag.NewPlanner(nil, nil, nil), dag.NewExecutor(sessions, nil, nil, nil, nil, nil), nil, nil, nil)
	o.SetArtifacts(artifact.InMemoryService())
	for _, err := range o.Run(context.Background(), "u1", "c1", SourceApp, "show me", nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if stub.offered {
		t.Fatal("render_ui offered to the orchestrator without orchestrator.tools listing it")
	}
}

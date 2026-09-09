package agent

import (
	"context"
	"iter"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

type saveArtifactArgs struct{}

// saveArtifactResult never surfaces a Go error to the tool framework (whose
// own error wire-shape isn't this test's concern) - OK/Text is checked
// directly out of the FunctionResponse by artifactWorkerModel below, so a
// nil ctx.Artifacts() (Artifacts() returning nil, or a nil-interface panic
// from calling a method on it) is unambiguously distinguishable from success.
type saveArtifactResult struct {
	OK   bool
	Text string
}

// newSaveArtifactTool round-trips a blob through tc.Artifacts(), reporting
// whether it worked - proving the worker's RunnerConfig actually got a live
// ArtifactService rather than a nil one.
func newSaveArtifactTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[saveArtifactArgs, saveArtifactResult](
		functiontool.Config{Name: "save_artifact", Description: "round-trips an artifact"},
		func(tc adkagent.Context, _ saveArtifactArgs) (res saveArtifactResult, _ error) {
			defer func() {
				if r := recover(); r != nil {
					res = saveArtifactResult{OK: false}
				}
			}()
			svc := tc.Artifacts()
			if svc == nil {
				return saveArtifactResult{OK: false}, nil
			}
			part := &genai.Part{InlineData: &genai.Blob{Data: []byte("hello-artifact"), MIMEType: "text/plain"}}
			if _, err := svc.Save(tc, "proof.txt", part); err != nil {
				return saveArtifactResult{OK: false}, nil
			}
			loaded, err := svc.Load(tc, "proof.txt")
			if err != nil || loaded == nil || loaded.Part == nil || loaded.Part.InlineData == nil {
				return saveArtifactResult{OK: false}, nil
			}
			return saveArtifactResult{OK: true, Text: string(loaded.Part.InlineData.Data)}, nil
		})
	if err != nil {
		t.Fatalf("save_artifact tool: %v", err)
	}
	return tl
}

type artifactWorkerModel struct{ calls int }

func (m *artifactWorkerModel) Name() string { return "artifact-worker-model" }
func (m *artifactWorkerModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	sawResult, ok := false, false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				sawResult = true
				if v, _ := p.FunctionResponse.Response["OK"].(bool); v {
					ok = true
				}
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if sawResult {
			text := "failed"
			if ok {
				text = "done"
			}
			yield(&model.LLMResponse{
				Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "save_artifact"}},
			}},
			TurnComplete: true,
		}, nil)
	}
}

// TestServeSetsWorkerArtifactService is a regression test for the ADK
// audit's A7 finding: agent.Serve set SessionService/MemoryService/
// Compaction on the worker's RunnerConfig but never ArtifactService, so
// ctx.Artifacts() was nil in every worker tool/callback even when the
// caller (internal/serve) had a live artifact.Service to give it - the DAG
// and orchestrator runners already got one (nativegraph.go, orchestrator.go).
func TestServeSetsWorkerArtifactService(t *testing.T) {
	worker, err := llmagent.New(llmagent.Config{
		Name: "artifact-worker", Description: "w", Model: &artifactWorkerModel{},
		Tools: []tool.Tool{newSaveArtifactTool(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(worker, session.InMemoryService(), nil, artifact.InMemoryService(), Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client, err := srv.ClientForNode("test-node", "test-ctx")
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		AppName: "spike", Agent: client, SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawDone bool
	for ev, err := range r.Run(context.Background(), "u", "s1", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text == "done" {
				sawDone = true
			}
		}
	}
	if !sawDone {
		t.Fatal("worker never reached its final answer - save_artifact tool likely failed (ctx.Artifacts() nil)")
	}
}

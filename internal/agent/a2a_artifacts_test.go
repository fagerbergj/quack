package agent

import (
	"context"
	"slices"
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

// saveArtifactResult never surfaces a Go error; artifactWorkerModel reads OK from the FunctionResponse,
// so a nil ctx.Artifacts() is distinguishable from success.
type saveArtifactResult struct {
	OK   bool
	Text string
}

// newSaveArtifactTool round-trips a blob through tc.Artifacts(), proving the worker got a live ArtifactService.
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

// artifactWorkerModel calls save_artifact, then answers "done" or "failed" by the tool's OK flag.
var artifactWorkerModel = &fakeLLM{func(req *model.LLMRequest) *model.LLMResponse {
	frs := funcResponses(req)
	if len(frs) == 0 {
		return turn(&genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "save_artifact"}})
	}
	if slices.ContainsFunc(frs, func(fr *genai.FunctionResponse) bool { v, _ := fr.Response["OK"].(bool); return v }) {
		return turn(&genai.Part{Text: "done"})
	}
	return turn(&genai.Part{Text: "failed"})
}}

// Serve must set ArtifactService on the worker's RunnerConfig, or ctx.Artifacts() is nil in every worker tool.
func TestServeSetsWorkerArtifactService(t *testing.T) {
	worker, err := llmagent.New(llmagent.Config{
		Name: "artifact-worker", Description: "w", Model: artifactWorkerModel,
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

package dag

import (
	"context"
	"iter"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// saveLoadArgs/Result: the round-trip tool proves ctx.Artifacts() inside a
// gated node is the exact instance Executor.SetArtifacts wired in - not a
// separate/nil service - by saving then immediately loading a blob.
type saveLoadArgs struct{}
type saveLoadResult struct{ Text string }

func newSaveLoadTool(t *testing.T, got chan<- saveLoadResult) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[saveLoadArgs, saveLoadResult](
		functiontool.Config{Name: "save_load", Description: "round-trips an artifact"},
		func(tc adkagent.Context, _ saveLoadArgs) (saveLoadResult, error) {
			part := &genai.Part{InlineData: &genai.Blob{Data: []byte("hello-artifact"), MIMEType: "text/plain"}}
			if _, err := tc.Artifacts().Save(tc, "proof.txt", part); err != nil {
				return saveLoadResult{}, err
			}
			loaded, err := tc.Artifacts().Load(tc, "proof.txt")
			if err != nil {
				return saveLoadResult{}, err
			}
			res := saveLoadResult{Text: string(loaded.Part.InlineData.Data)}
			got <- res
			return res, nil
		})
	if err != nil {
		t.Fatalf("save_load tool: %v", err)
	}
	return tl
}

// artifactStub calls save_load once, then returns a fixed final answer - the
// proof is the tool's own Save+Load round-trip, read off the got channel below.
type artifactStub struct {
	mu    sync.Mutex
	calls int
}

func (*artifactStub) Name() string { return "artifactStub" }
func (s *artifactStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		if n == 1 {
			yield(gCall("save_load", map[string]any{}), nil)
			return
		}
		yield(gText("done"), nil)
	}
}

// TestRunPlanAsGraph_ArtifactServiceReachableAtNodeLevel: with
// Executor.SetArtifacts wired, a node's ctx.Artifacts() is live and usable -
// answers the "does anything actually consume it" question with a real Save+Load.
func TestRunPlanAsGraph_ArtifactServiceReachableAtNodeLevel(t *testing.T) {
	stub := &artifactStub{}
	got := make(chan saveLoadResult, 1)
	worker, err := llmagent.New(llmagent.Config{
		Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer.",
		Tools: []tool.Tool{newSaveLoadTool(t, got)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": worker}, map[string]model.LLM{"w": stub},
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	ex.SetArtifacts(artifact.InMemoryService())
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "do it"}}}

	runPlanSSE(t, ex, plan, "chat")

	select {
	case res := <-got:
		if res.Text != "hello-artifact" {
			t.Errorf("round-tripped artifact text = %q, want %q", res.Text, "hello-artifact")
		}
	default:
		t.Fatal("save_load tool never ran - ctx.Artifacts() unreachable or unwired")
	}
}

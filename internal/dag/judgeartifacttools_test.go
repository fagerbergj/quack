// #1497: the judge only ever held jail-scoped repo read tools (no
// list_artifacts/read_artifact), so an answer pointing at an artifact scored
// zero for grounding. Drives a gated node and checks the judge's own read_artifact reaches the worker's chat-scoped store.
package dag_test

import (
	"context"
	"iter"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// jatWorkerStub answers immediately, no tools - the artifact under test is
// seeded directly (as the worker's own write_artifact tool would have left it).
type jatWorkerStub struct{}

func (jatWorkerStub) Name() string { return "jatWorkerStub" }

func (jatWorkerStub) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(lcText("see the artifact I wrote"), nil)
	}
}

// jatJudgeStub calls read_artifact(id) once, records what came back, then
// passes - proving the judge round's own tool actually reached the store.
type jatJudgeStub struct {
	id  string
	got *string
}

func (jatJudgeStub) Name() string { return "jatJudgeStub" }

func (j jatJudgeStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if s, ok := jatFuncResponseResult(req, "read_artifact"); ok {
			*j.got = s
			yield(lcCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		yield(lcCall("read_artifact", map[string]any{"id": j.id}), nil)
	}
}

// jatFuncResponseResult extracts a functiontool's wrapped {"result": ...}
// string reply for the named tool call from req's prior turns.
func jatFuncResponseResult(req *model.LLMRequest, name string) (string, bool) {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				s, ok := p.FunctionResponse.Response["result"].(string)
				return s, ok
			}
		}
	}
	return "", false
}

// TestGatedNodeJudgeReadsChatScopedArtifact: a worker artifact seeded under
// (appName, userID, chatID) must be exactly what the SAME node's judge round
// reads back - its recordstore.Client is built from those same coordinates (buildGateNodes), not a separate/unscoped one.
func TestGatedNodeJudgeReadsChatScopedArtifact(t *testing.T) {
	const userID = "u"
	const chatID = "judge-artifact-chat"
	const content = "seeded research findings"

	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, artifactref.AppName, userID, chatID)
	id, _, err := rc.SaveBlob(context.Background(), "text", []byte(content), "text/plain", "hint",
		recordstore.Lineage{NodeID: "n1", Author: "worker"})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	workerModel := inference.TracedModelForTesting(jatWorkerStub{}, "jat-worker-model")
	worker, err := llmagent.New(llmagent.Config{Name: "w", Model: workerModel, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	scoped := lcScopedAgent{Agent: worker, model: workerModel}

	var got string
	ex := dag.NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{"w": scoped},
		map[string]model.LLM{"w": workerModel},
		vetting.NewJudgeFactory(jatJudgeStub{id: id, got: &got}, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} },
		nil)
	ex.SetArtifacts(svc)

	plan := dag.Plan{ID: "t", UserMessage: "research something", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "answer"}}}
	genContent := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", userID, chatID, genContent,
		func(stream.SSEEvent, error) bool { return true }, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got != content {
		t.Errorf("judge's read_artifact(%s) = %q, want %q", id, got, content)
	}
}

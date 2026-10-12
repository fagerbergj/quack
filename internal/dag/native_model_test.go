package dag_test

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// A native worker's model crosses A2A, so its round's agent_complete and the node's node_done name it.
func TestNativeWorker_RoundAndNodeNameTheModel(t *testing.T) {
	testNativeWorkerSessions = session.InMemoryService()
	stub := fnLLM(func(context.Context, *model.LLMRequest) *model.LLMResponse {
		r := atText("The answer, long enough to clear the gate's length floor without any trouble at all.")
		r.ModelVersion = "stub-v1"
		return r
	})
	worker, err := llmagent.New(llmagent.Config{Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo"})
	if err != nil {
		t.Fatal(err)
	}
	plan := dag.Plan{ID: "p1", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"}}}
	ex := dag.NewExecutor(testNativeWorkerSessions, map[string]adkagent.Agent{"solo": testNativeWorker{Agent: worker}}, nil,
		vetting.NewJudgeFactory(passJudge, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	var roundModel, nodeModel string
	sink := func(ev stream.SSEEvent, _ error) bool {
		switch d := ev.Data.(type) {
		case stream.AgentCompleteData:
			if d.Stage == stream.StageWorker {
				roundModel = d.Model
			}
		case stream.NodeDoneData:
			nodeModel = d.Model
		}
		return true
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "chat1", content, sink, map[string]string{}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if roundModel != "stub-v1" || nodeModel != "stub-v1" {
		t.Errorf("worker agent_complete model = %q, node_done model = %q; want stub-v1 for both", roundModel, nodeModel)
	}
}

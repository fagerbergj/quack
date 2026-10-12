package orchestrator

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// A retry given guidance hands the node its task with the guidance appended.
func TestRetryNode_AppendsGuidanceToTheTask(t *testing.T) {
	var mu sync.Mutex
	var prompts strings.Builder
	m := funcModel(func(_ context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
		mu.Lock()
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				prompts.WriteString(p.Text)
			}
		}
		mu.Unlock()
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(&model.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel)}, nil)
		}
	})
	w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m}, nil,
		func(context.Context, string) vetting.Config { return vetting.Config{} }, nil)
	orch := New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	planJSON, _ := json.Marshal(dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "t"}}})
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "chat",
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}
	for range orch.RetryNode(context.Background(), "u", "chat", "p", nil, "n1", "FOCUS-ON-X") {
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(prompts.String(), "[Retry guidance]: FOCUS-ON-X") {
		t.Errorf("worker prompts never carried the guidance:\n%s", prompts.String())
	}
}

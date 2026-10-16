package dag

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// wedgedModel fails the way a wedged ACP agent does: an error wrapping inference.ErrAgentSilent.
type wedgedModel struct{}

func (wedgedModel) Name() string { return "wedged" }

func (wedgedModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, fmt.Errorf("acp: no activity for 10m0s - treating the ACP agent as wedged (%w)", inference.ErrAgentSilent))
	}
}

// TestExecute_WedgedAgentNamesTheCause: the wedge survives ADK's %v re-wrap between scheduler levels.
func TestExecute_WedgedAgentNamesTheCause(t *testing.T) {
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: wedgedModel{}, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	judge := fixedLLM("", nil)
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": ag}, map[string]model.LLM{"w": wedgedModel{}},
		vetting.NewJudgeFactory(judge, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "do it"}}}

	var events []stream.SSEEvent
	var mu sync.Mutex
	yield := func(ev stream.SSEEvent, _ error) bool {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		return true
	}
	ctx := stream.WithYield(context.Background(), func(ev stream.SSEEvent) { yield(ev, nil) })
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}
	_, _ = ex.RunPlanAsGraph(ctx, plan, "quack", "u", "chat", content, yield, map[string]string{}, nil)
	for _, ev := range events {
		if d, ok := ev.Data.(stream.NodeFailedData); ok && d.NodeID == "n1" {
			if !strings.Contains(d.Error, "agent went silent past its idle timeout") {
				t.Fatalf("node_failed error = %q, want the wedge named", d.Error)
			}
			return
		}
	}
	t.Fatal("wedged node should surface as node_failed")
}

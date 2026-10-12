package orchestrator

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// A cut run's answer is its sink's output, delivered once per turn however often boot finishes it.
func TestFinishCutRun_DeliversTheSinkOnce(t *testing.T) {
	ctx := stream.WithTurnID(context.Background(), "turn-1")
	sessions := session.InMemoryService()
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "c"}); err != nil {
		t.Fatal(err)
	}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{}, nil, nil, func(context.Context, string) vetting.Config { return vetting.Config{} }, nil)
	o := New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	plan := dag.Plan{ID: "p", Nodes: []dag.Node{{ID: "n1"}, {ID: "n2", DependsOn: []string{"n1"}}}}
	for range 2 {
		o.FinishCutRun(ctx, "u", "c", plan, map[string]string{"n1": "N1", "n2": "N2-ANSWER"})
	}
	var answers []string
	for _, ev := range o.PriorEvents(ctx, "u", "c") {
		if ev.CustomMetadata[stream.DeliveredAnswerMeta] == "turn-1" {
			answers = append(answers, answerText(ev))
		}
	}
	if len(answers) != 1 || answers[0] != "N2-ANSWER" {
		t.Errorf("delivered answers = %q, want n2's output once", answers)
	}
}

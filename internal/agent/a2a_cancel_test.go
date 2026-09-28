package agent

import (
	"context"
	"iter"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// untilCancelled blocks each call until its ctx ends, then records that it returned.
type untilCancelled struct {
	entered, exited chan struct{}
}

func (untilCancelled) Name() string { return "until-cancelled" }

func (m untilCancelled) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		close(m.entered)
		defer close(m.exited)
		select {
		case <-ctx.Done():
			yield(nil, ctx.Err())
		case <-time.After(30 * time.Second):
		}
	}
}

// a2a-go runs the worker on a detached ctx, so a cancelled node must stop it
// explicitly; the client may only return once the worker has.
func TestA2AClientCancelStopsWorker(t *testing.T) {
	m := untilCancelled{entered: make(chan struct{}), exited: make(chan struct{})}
	worker, err := llmagent.New(llmagent.Config{Name: "blocker", Model: m, Description: "blocks", Instruction: "block"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(worker, session.InMemoryService(), nil, nil, Compaction{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	client, err := srv.ClientForNode("n", "cancel-ctx")
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "spike", Agent: client, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-m.entered; cancel() }()
	for range r.Run(ctx, "local", "s1", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
	}
	select {
	case <-m.exited:
	default:
		t.Fatal("client returned while the worker's model call was still running")
	}
}

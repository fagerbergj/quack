package adkdebug

import (
	"context"
	"encoding/json"
	"iter"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
)

// echoModel is the smallest model.LLM that lets a real runner.Run happen,
// standing in for the fake-model test infra (internal/inference.NewReplayModel)
// that needs a recorded bundle - this test only needs one turn.
type echoModel struct{}

func (echoModel) Name() string { return "echo" }

func (echoModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{genai.NewPartFromText("ok")}},
			TurnComplete: true,
		}, nil)
	}
}

// TestMount_TraceFromRealRun proves the memo's open question: with the
// mount's SpanProcessor registered onto the live TracerProvider, a real
// agent run (not a mock) populates /debug/trace/session/{id}.
func TestMount_TraceFromRealRun(t *testing.T) {
	ag, err := llmagent.New(llmagent.Config{Name: "echo", Model: echoModel{}, Description: "test agent"})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	sessions := session.InMemoryService()
	mount, err := New(sessions, map[string]adkagent.Agent{"echo": ag}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tp := sdktrace.NewTracerProvider()
	tp.RegisterSpanProcessor(mount.SpanProcessor())
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prevTP) })

	r, err := runner.New(runner.Config{
		AppName: "echo", Agent: ag, SessionService: sessions, AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	const sessionID = "test-session"
	msg := &genai.Content{Role: "user", Parts: []*genai.Part{genai.NewPartFromText("hi")}}
	for _, rerr := range r.Run(context.Background(), "u", sessionID, msg, adkagent.RunConfig{}) {
		if rerr != nil {
			t.Fatalf("run: %v", rerr)
		}
	}
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/debug/trace/session/"+sessionID, nil)
	rec := httptest.NewRecorder()
	mount.Handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GET /api/debug/trace/session/%s: status %d, body %s", sessionID, rec.Code, rec.Body.String())
	}
	var spans []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spans); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	if len(spans) == 0 {
		t.Fatalf("expected at least one span from the real run, got none (body: %s)", rec.Body.String())
	}
}

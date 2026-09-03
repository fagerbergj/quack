package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// failingListService: List always errors, everything else delegates - stands
// in for a real store outage (e.g. the artifact-DB connection down).
type failingListService struct{ artifact.Service }

func (failingListService) List(context.Context, *artifact.ListRequest) (*artifact.ListResponse, error) {
	return nil, errors.New("artifact store unreachable")
}

// TestFailSoftListArtifacts_DegradesToEmpty proves a List failure (which would
// otherwise fail the whole orchestrator turn via loadartifactstool) degrades
// to "no artifacts offered" instead of propagating.
func TestFailSoftListArtifacts_DegradesToEmpty(t *testing.T) {
	wrapped := failSoftListArtifacts{failingListService{artifact.InMemoryService()}}
	resp, err := wrapped.List(context.Background(), &artifact.ListRequest{AppName: AppName, UserID: "u1", SessionID: "c1"})
	if err != nil {
		t.Fatalf("List: %v, want nil error (fail-soft)", err)
	}
	if len(resp.FileNames) != 0 {
		t.Fatalf("FileNames = %v, want empty on a failed List", resp.FileNames)
	}
}

// TestFailSoftListArtifacts_LoadBounded proves load_artifacts (the ADK-native
// read path) rejects an oversized artifact instead of dumping it into model
// context unbounded (#1006 item 7) - the same cap shape read_artifact (ACP,
// internal/acp/memorymcp.go readArtifactMaxBytes) already enforces.
func TestFailSoftListArtifacts_LoadBounded(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	big := make([]byte, loadArtifactMaxBytes+1)
	if _, err := svc.Save(ctx, &artifact.SaveRequest{
		AppName: AppName, UserID: "u1", SessionID: "c1", FileName: "big.bin",
		Part: genai.NewPartFromBytes(big, "application/octet-stream"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, &artifact.SaveRequest{
		AppName: AppName, UserID: "u1", SessionID: "c1", FileName: "small.txt",
		Part: genai.NewPartFromBytes([]byte("fits fine"), "text/plain"),
	}); err != nil {
		t.Fatal(err)
	}
	wrapped := failSoftListArtifacts{svc}

	if _, err := wrapped.Load(ctx, &artifact.LoadRequest{AppName: AppName, UserID: "u1", SessionID: "c1", FileName: "big.bin"}); err == nil {
		t.Fatal("Load of an oversized artifact should error, not return the bytes")
	}
	resp, err := wrapped.Load(ctx, &artifact.LoadRequest{AppName: AppName, UserID: "u1", SessionID: "c1", FileName: "small.txt"})
	if err != nil {
		t.Fatalf("Load small.txt: %v", err)
	}
	if string(resp.Part.InlineData.Data) != "fits fine" {
		t.Fatalf("small.txt content = %q", resp.Part.InlineData.Data)
	}
}

// loadArtifactsStub: first call requests load_artifacts, second call replies
// with whatever text the tool's result carried back on the request.
type loadArtifactsStub struct {
	mu    sync.Mutex
	calls int
}

func (*loadArtifactsStub) Name() string { return "loadArtifactsStub" }

// lastRequestText flattens everything the load_artifacts tool appended to the
// request (loadIndividualArtifact puts the loaded bytes in InlineData, not Text).
func lastRequestText(req *model.LLMRequest) string {
	var sb strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			sb.WriteString(p.Text)
			if p.InlineData != nil {
				sb.Write(p.InlineData.Data)
			}
		}
	}
	return sb.String()
}

func (s *loadArtifactsStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.mu.Unlock()
		if n == 1 {
			yield(&model.LLMResponse{
				Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{Name: "load_artifacts", Args: map[string]any{"artifact_names": []any{"notes.txt"}}},
				}}},
				FinishReason: genai.FinishReasonStop, TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "saw: " + lastRequestText(req)}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

// TestOrchestratorRun_LoadArtifactsTool proves that once SetArtifacts is
// wired, the orchestrator's own runner can list and load a session artifact
// through the load_artifacts tool - end to end via Run, not just at
// construction time.
func TestOrchestratorRun_LoadArtifactsTool(t *testing.T) {
	ctx := context.Background()
	svc := artifact.InMemoryService()
	const userID, chatID = "u1", "c1"
	if _, err := svc.Save(ctx, &artifact.SaveRequest{
		AppName: AppName, UserID: userID, SessionID: chatID, FileName: "notes.txt",
		Part: genai.NewPartFromBytes([]byte("hello artifact content"), "text/plain"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sessions := session.InMemoryService()
	o := New(sessions, &loadArtifactsStub{}, "you are the orchestrator", dag.NewPlanner(nil, nil, nil), dag.NewExecutor(sessions, nil, nil, nil, nil, nil), nil, nil, nil)
	o.SetArtifacts(svc)

	var texts []string
	for ev, err := range o.Run(ctx, userID, chatID, SourceApp, "please load notes.txt", nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		switch d := ev.Data.(type) {
		case stream.AgentTokenData:
			texts = append(texts, d.Text)
		case stream.AgentToolResultData:
			texts = append(texts, fmt.Sprint(d.Result))
		}
	}
	joined := strings.Join(texts, " ")
	if !strings.Contains(joined, "hello artifact content") {
		t.Fatalf("orchestrator run output = %q, want it to contain the loaded artifact's content", joined)
	}
}

// TestOrchestratorRun_NoArtifactService_ToolAbsentNoPanic: an orchestrator
// with no artifact service wired must not offer load_artifacts, and must not
// panic when a run happens regardless.
func TestOrchestratorRun_NoArtifactService_ToolAbsentNoPanic(t *testing.T) {
	ctx := context.Background()
	sessions := session.InMemoryService()
	textModel := scriptedModel{reply: "no tools needed"}
	o := New(sessions, textModel, "you are the orchestrator", dag.NewPlanner(nil, nil, nil), dag.NewExecutor(sessions, nil, nil, nil, nil, nil), nil, nil, nil)
	// o.artifacts left nil deliberately.

	for ev, err := range o.Run(ctx, "u1", "c1", SourceApp, "hello", nil) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		_ = ev
	}
}

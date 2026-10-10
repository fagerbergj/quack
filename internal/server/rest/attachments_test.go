package rest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/vetting"
)

// attachStub plays the orchestrator (routes on create_plan's presence), the judge, and the "media"
// worker, recording the bytes and mime the worker received.
type attachStub struct {
	mu          sync.Mutex
	workerCalls int
	seenBytes   []byte
	seenMime    string
}

func (*attachStub) Name() string { return "attachStub" }

func (s *attachStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case attachStubHasTool(req, "submit_verdict"):
			yield(attachStubCall("submit_verdict", map[string]any{"score": 0.95, "feedback": ""}), nil)
			return
		case !attachStubHasTool(req, "create_plan"): // no create_plan tool ⇒ this is the media worker
			s.mu.Lock()
			s.workerCalls++
			for _, c := range req.Contents {
				if c == nil {
					continue
				}
				for _, p := range c.Parts {
					if p != nil && p.InlineData != nil {
						s.seenBytes = append([]byte(nil), p.InlineData.Data...)
						s.seenMime = p.InlineData.MIMEType
					}
				}
			}
			s.mu.Unlock()
			yield(attachStubText("the image shows a duck"), nil)
			return
		}
		if id, ok := attachStubPlanID(req); ok {
			yield(attachStubCall("execute", map[string]any{"plan_id": id}), nil)
			return
		}
		// delivery is declared so the plan is complete (#slice3) - execute
		// ends the turn once it runs, matching this stub's single-shot flow.
		yield(attachStubCall("create_plan", map[string]any{
			"assignments": []any{map[string]any{"agent": "media", "task": "describe the attached image"}},
			"delivery":    map[string]any{"kind": "comment"},
		}), nil)
	}
}

func attachStubHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func attachStubPlanID(req *model.LLMRequest) (string, bool) {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != "create_plan" {
				continue
			}
			if id, ok := p.FunctionResponse.Response["plan_id"].(string); ok && id != "" {
				return id, true
			}
		}
	}
	return "", false
}

func attachStubText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop, TurnComplete: true,
	}
}

func attachStubCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}}},
		FinishReason: genai.FinishReasonStop, TurnComplete: true,
	}
}

// newAttachmentTestHandler builds a Handler with a real sqlite store, a row-backed artifact service,
// and a one-node "media" DAG.
func newAttachmentTestHandler(t *testing.T, dbPath string, stub *attachStub) *Handler {
	t.Helper()
	st, err := store.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	artifactSvc, err := st.RowArtifactService()
	if err != nil {
		t.Fatalf("RowArtifactService: %v", err)
	}
	st.SetArtifactService(artifactSvc)
	artifacts := store.NewTurnAwareService(artifactSvc)

	// Only the worker's model is wrapped with hydration, as in production:
	// the orchestrator's model never sees attachment parts directly.
	hydratedStub := inference.HydratingModelForTesting(stub, artifacts)
	worker, err := llmagent.New(llmagent.Config{
		Name: "media", Model: hydratedStub, Description: "reads images", Instruction: "ROLE:media Describe the attached image.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{"media": worker}, map[string]model.LLM{"media": hydratedStub},
		vetting.NewJudgeFactory(stub, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.5, JudgeRounds: 1} },
		map[string]bool{"media": true})
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "media", Description: "reads images"}}, nil, nil)
	orch := orchestrator.New(st.Sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)
	return NewHandler(st, orch, nil, nil, nil, nil, "test", nil, nil, artifacts, nil)
}

func postMultipart(t *testing.T, h *Handler, chatID, content, filename, mimeType string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("content", content); err != nil {
		t.Fatalf("write content field: %v", err)
	}
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, filename)},
		"Content-Type":        {mimeType},
	})
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+chatID+"/responses", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.SendChatMessage(rec, req, chatID)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST multipart: status %d body=%s", rec.Code, rec.Body.String())
	}
}

// fakePNG is never decoded, but distinctive enough to prove a byte-for-byte round trip
// and to search the persisted JSON for.
var fakePNG = []byte("\x89PNG-fake-pixel-data-0123456789abcdef")

// TestAttachmentRoundTrip: an attachment reaches the media worker's model as real bytes, while everything
// persisted (DAG plan, ADK session events) carries only a reference.
func TestAttachmentRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	stub := &attachStub{}
	h := newAttachmentTestHandler(t, dbPath, stub)
	chatID := mustCreateChat(t, h)

	postMultipart(t, h, chatID, "what is in this image?", "photo.png", "image/png", fakePNG)

	stub.mu.Lock()
	gotBytes, gotMime, calls := stub.seenBytes, stub.seenMime, stub.workerCalls
	stub.mu.Unlock()
	if calls == 0 {
		t.Fatal("the media worker was never invoked")
	}
	if !bytes.Equal(gotBytes, fakePNG) {
		t.Errorf("worker saw bytes %q, want the original attachment %q", gotBytes, fakePNG)
	}
	if gotMime != "image/png" {
		t.Errorf("worker saw mime %q, want image/png", gotMime)
	}

	// Persisted plan carries no blob bytes.
	ctx := context.Background()
	dp, err := h.store.GetLatestDagPlan(ctx, chatID)
	if err != nil || dp == nil {
		t.Fatalf("GetLatestDagPlan: %v", err)
	}
	if bytes.Contains([]byte(dp.PlanJSON), fakePNG) {
		t.Errorf("persisted DAG plan JSON contains the raw attachment bytes:\n%s", dp.PlanJSON)
	}
	if bytes.Contains([]byte(dp.PlanJSON), []byte(base64.StdEncoding.EncodeToString(fakePNG))) {
		t.Errorf("persisted DAG plan JSON contains the base64-encoded attachment bytes:\n%s", dp.PlanJSON)
	}

	// Persisted ADK session events carry no blob bytes either.
	userID := h.sessionUser(ctx, chatID)
	resp, err := h.store.Sessions.Get(ctx, &session.GetRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID})
	if err != nil {
		t.Fatalf("Sessions.Get: %v", err)
	}
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.InlineData != nil && bytes.Equal(p.InlineData.Data, fakePNG) {
				t.Fatalf("a session event (author=%s) carries the raw attachment bytes", ev.Author)
			}
		}
	}

	// The attachment lists as a "bytes" recordstore artifact with kind/class/lineage set.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/artifacts", nil)
	listRec := httptest.NewRecorder()
	h.ListChatArtifacts(listRec, listReq, chatID)
	var list schema.ArtifactList
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode artifact list: %v", err)
	}
	var found *schema.ArtifactSummary
	for i := range list.Data {
		if list.Data[i].Name == "bytes:upload-photo.png" {
			found = &list.Data[i]
		}
	}
	if found == nil {
		t.Fatalf("no bytes:upload-photo.png artifact in %+v", list.Data)
	}
	if found.Kind == nil || *found.Kind != "bytes" {
		t.Errorf("kind = %v, want \"bytes\"", found.Kind)
	}
	if found.Class == nil || *found.Class != "blob" {
		t.Errorf("class = %v, want \"blob\"", found.Class)
	}
	if found.Lineage == nil || found.Lineage.Author == nil || *found.Lineage.Author != "user" {
		t.Errorf("lineage = %+v, want author \"user\"", found.Lineage)
	}
}

// TestAttachmentRoundTrip_SecondAccessStillHydrates: loading the same artifact revision again
// (as a later turn would) still resolves to the original bytes.
func TestAttachmentRoundTrip_SecondAccessStillHydrates(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")
	stub := &attachStub{}
	h := newAttachmentTestHandler(t, dbPath, stub)
	chatID := mustCreateChat(t, h)

	postMultipart(t, h, chatID, "what is in this image?", "photo.png", "image/png", fakePNG)
	stub.mu.Lock()
	stub.seenBytes, stub.seenMime, stub.workerCalls = nil, "", 0
	stub.mu.Unlock()

	// Simulate a later turn's model call that references the same
	// artifact revision again, through the same hydrating wrapper prod uses.
	userID := h.sessionUser(context.Background(), chatID)
	ref := artifactref.Encode(userID, chatID, "bytes:upload-photo.png", 1, "image/png")
	rec := &recordingModel{}
	hydrated := inference.HydratingModelForTesting(rec, h.artifacts)
	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{ref}}}}
	for range hydrated.GenerateContent(context.Background(), req, false) {
	}
	if rec.got == nil {
		t.Fatal("the wrapped model was never called")
	}
	got := rec.got.Contents[0].Parts[0]
	if got.InlineData == nil {
		t.Fatal("second hydration did not resolve the reference to real bytes")
	}
	if !bytes.Equal(got.InlineData.Data, fakePNG) {
		t.Errorf("second hydration got %q, want %q", got.InlineData.Data, fakePNG)
	}
}

// recordingModel records the request hydratingModel delegates: GenerateContent must not mutate
// the caller's req, which the model.call ledger later logs.
type recordingModel struct{ got *model.LLMRequest }

func (m *recordingModel) Name() string { return "recording" }

func (m *recordingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.got = req
	return func(yield func(*model.LLMResponse, error) bool) { yield(attachStubText("ok"), nil) }
}

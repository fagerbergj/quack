package inference

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
)

// seedArtifact saves data under name in svc and returns a reference part for it.
func seedArtifact(t *testing.T, svc artifact.Service, userID, sessionID, name, mimeType string, data []byte) *genai.Part {
	t.Helper()
	resp, err := svc.Save(context.Background(), &artifact.SaveRequest{
		AppName: artifactref.AppName, UserID: userID, SessionID: sessionID, FileName: name,
		Part: &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: mimeType}},
	})
	if err != nil {
		t.Fatalf("seed artifact %q: %v", name, err)
	}
	return artifactref.Encode(userID, sessionID, name, resp.Version, mimeType)
}

func TestHydratingModel_ReplacesReferenceWithRealBytes(t *testing.T) {
	svc := artifact.InMemoryService()
	ref := seedArtifact(t, svc, "u", "s", "photo.png", "image/png", []byte("pixels"))

	rec := &recordingLLM{}
	h := HydratingModelForTesting(rec, svc)
	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "describe"}, ref}}}}
	for range h.GenerateContent(context.Background(), req, false) {
	}
	if rec.got == nil {
		t.Fatal("inner model was never called")
	}
	parts := rec.got.Contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if parts[0].Text != "describe" {
		t.Errorf("part 0 = %+v, want the untouched text part", parts[0])
	}
	if parts[1].InlineData == nil || string(parts[1].InlineData.Data) != "pixels" || parts[1].InlineData.MIMEType != "image/png" {
		t.Errorf("part 1 = %+v, want hydrated InlineData{pixels, image/png}", parts[1])
	}
}

// recordingLLM is model.LLM that just records the request it was given.
type recordingLLM struct{ got *model.LLMRequest }

func (*recordingLLM) Name() string { return "recording" }

func (r *recordingLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	r.got = req
	return func(yield func(*model.LLMResponse, error) bool) { yield(&model.LLMResponse{}, nil) }
}

// TestHydratingModel_DoesNotMutateCallerRequest is the regression test for
// the ledger-leak bug: tracedModel (factory.go) wraps hydratingModel and
// logs the SAME req pointer via emitChatEvent after GenerateContent returns.
// If hydration mutated req.Contents in place, that log would carry real bytes.
func TestHydratingModel_DoesNotMutateCallerRequest(t *testing.T) {
	svc := artifact.InMemoryService()
	ref := seedArtifact(t, svc, "u", "s", "photo.png", "image/png", []byte("pixels"))

	inner := &stubModel{name: "m", resps: []*model.LLMResponse{{}}}
	h := HydratingModelForTesting(inner, svc)

	content := &genai.Content{Role: "user", Parts: []*genai.Part{ref}}
	req := &model.LLMRequest{Contents: []*genai.Content{content}}
	for range h.GenerateContent(context.Background(), req, false) {
	}

	if req.Contents[0] != content {
		t.Fatalf("hydration replaced req.Contents[0] in place - the caller's Content object must be left untouched")
	}
	got := req.Contents[0].Parts[0]
	if got != ref {
		t.Fatalf("hydration replaced the reference part in place: %+v", got)
	}
	if got.InlineData != nil {
		t.Fatalf("the caller's request now carries real bytes: %+v", got)
	}
	if _, _, _, _, ok := artifactref.Decode(got); !ok {
		t.Fatalf("the caller's part is no longer a valid reference: %+v", got)
	}
}

func TestHydratingModel_MissingArtifact_LeavesReferenceInPlace(t *testing.T) {
	svc := artifact.InMemoryService() // never saved to - Load always fails
	ref := artifactref.Encode("u", "s", "missing.png", 1, "image/png")

	rec := &recordingLLM{}
	h := HydratingModelForTesting(rec, svc)
	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{ref}}}}
	for range h.GenerateContent(context.Background(), req, false) {
	}

	if rec.got.Contents[0].Parts[0].FileData == nil {
		t.Errorf("a failed hydration must leave the reference in place, not drop the part")
	}
}

func TestHydratingModel_NilArtifactService_NoOp(t *testing.T) {
	rec := &recordingLLM{}
	h := HydratingModelForTesting(rec, nil)
	ref := artifactref.Encode("u", "s", "n.png", 1, "image/png")
	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{ref}}}}
	for range h.GenerateContent(context.Background(), req, false) {
	}
	if rec.got != req {
		t.Errorf("with no artifact service, the request must pass through unchanged")
	}
}

func TestHydratingModel_EmbedDelegates(t *testing.T) {
	want := [][]float32{{1, 2, 3}}
	inner := &embeddableStub{stubModel: stubModel{name: "embed-model"}, vectors: want}
	h := HydratingModelForTesting(inner, artifact.InMemoryService())
	got, err := h.(interface {
		Embed(context.Context, []string) ([][]float32, error)
	}).Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Embed() = %v, want %v", got, want)
	}
}

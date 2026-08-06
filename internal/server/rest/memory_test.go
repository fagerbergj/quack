package rest

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/schema"
)

// fixedEmbedder returns the same unit vector for every text - enough to
// round-trip a memory through Commit/List/Search without a real embedding model.
type fixedEmbedder struct{}

func (fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// addConsolidator ADDs whatever candidates it's given, verbatim - a stand-in
// for the real consolidator LLM so Commit can write test fixtures.
type addConsolidator struct{}

func (addConsolidator) Name() string { return "add-consolidator" }

func (addConsolidator) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	reply := `{"ops":[{"action":"ADD","content":"NightsOut needs minSdk 30 for DEX 040","kind":"repo"}]}`
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: reply}}}}, nil)
	}
}

func newTestMemStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "mem.db"), fixedEmbedder{}, addConsolidator{}, "test_task", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return s
}

// No memory backend configured: listing is empty (not an error), and forgetting anything 404s.
func TestListMemories_NoStoreConfigured(t *testing.T) {
	h := newTestHandler(t) // taskMem/userMem both nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memories", nil)
	w := httptest.NewRecorder()
	h.ListMemories(w, req, schema.ListMemoriesParams{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got schema.MemoryList
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 0 || len(got.Memories) != 0 {
		t.Fatalf("got %+v, want an empty list", got)
	}
}

func TestDeleteMemory_UnknownID_404(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/memories/does-not-exist", nil)
	w := httptest.NewRecorder()
	h.DeleteMemory(w, req, "does-not-exist")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// A committed memory shows up in a bucket listing, and forgetting it removes
// it - and only it - from that same listing.
func TestListAndDeleteMemory_RoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)

	if _, err := h.taskMem.Commit(ctx, memory.Scope{Repo: "NightsOut"}, "code-explorer",
		[]memory.Candidate{{Content: "needs minSdk 30", Metadata: map[string]string{"bucket": "repo"}}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	bucket := "repo:NightsOut"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memories?bucket="+bucket, nil)
	w := httptest.NewRecorder()
	h.ListMemories(w, req, schema.ListMemoriesParams{Bucket: &bucket})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got schema.MemoryList
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 || len(got.Memories) != 1 {
		t.Fatalf("got %+v, want exactly one memory", got)
	}
	id := got.Memories[0].Id

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/memories/"+id, nil)
	delW := httptest.NewRecorder()
	h.DeleteMemory(delW, delReq, id)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delW.Code)
	}

	w2 := httptest.NewRecorder()
	h.ListMemories(w2, httptest.NewRequest(http.MethodGet, "/api/v1/memories?bucket="+bucket, nil), schema.ListMemoriesParams{Bucket: &bucket})
	var got2 schema.MemoryList
	if err := json.NewDecoder(w2.Body).Decode(&got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got2.Total != 0 || len(got2.Memories) != 0 {
		t.Fatalf("after delete: got %+v, want empty", got2)
	}

	// Deleting it again is a 404 - it's really gone, not tombstoned.
	delW2 := httptest.NewRecorder()
	h.DeleteMemory(delW2, httptest.NewRequest(http.MethodDelete, "/api/v1/memories/"+id, nil), id)
	if delW2.Code != http.StatusNotFound {
		t.Fatalf("re-delete status = %d, want 404", delW2.Code)
	}
}

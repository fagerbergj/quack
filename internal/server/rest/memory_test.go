package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// echoConsolidator ADDs each staged candidate verbatim as its own point - a
// stand-in for the real consolidator LLM so Commit can write test fixtures
// with recognizable, distinct content. Reads only the STAGED CANDIDATES
// section of the prompt: the EXISTING MEMORIES section commit.go also sends
// (once a bucket is non-empty) uses the same "- " line prefix, and echoing
// those back too would re-ADD every prior fact on each subsequent Commit.
type echoConsolidator struct{}

func (echoConsolidator) Name() string { return "echo-consolidator" }

func (echoConsolidator) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var text strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			text.WriteString(p.Text)
		}
	}
	staged := text.String()
	if i := strings.Index(staged, "\nEXISTING MEMORIES"); i >= 0 {
		staged = staged[:i]
	}
	if i := strings.Index(staged, "\nFINAL ANSWER"); i >= 0 {
		staged = staged[:i]
	}
	var ops []string
	for _, line := range strings.Split(staged, "\n") {
		if content, ok := strings.CutPrefix(line, "- "); ok {
			ops = append(ops, `{"action":"ADD","content":"`+content+`","kind":"repo"}`)
		}
	}
	reply := `{"ops":[` + strings.Join(ops, ",") + `]}`
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: reply}}}}, nil)
	}
}

func newTestMemStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "mem.db"), fixedEmbedder{}, echoConsolidator{}, "test_task", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return s
}

// commitFact writes one recognizable fact into s via Commit (echoConsolidator
// ADDs it verbatim), so a test can seed multiple distinct entries and later
// assert on their content or relative order.
func commitFact(t *testing.T, s *memory.Store, bucket, content string) {
	t.Helper()
	if _, err := s.Commit(context.Background(), memory.Scope{Repo: bucket}, "test", memory.Provenance{},
		[]memory.Candidate{{Content: content, Metadata: map[string]string{"bucket": "repo"}}}, ""); err != nil {
		t.Fatalf("Commit(%q): %v", content, err)
	}
}

func memoryContents(mems []schema.Memory) []string {
	out := make([]string, len(mems))
	for i, m := range mems {
		out[i] = m.Content
	}
	return out
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

// A committed memory shows up in a bucket listing; deleting it invalidates it
// (design doc §4(b), soft-delete only) - it drops out of the default listing
// but is still there, reason "manual delete", under include_invalidated=true.
func TestListAndDeleteMemory_RoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)

	if _, err := h.taskMem.Commit(ctx, memory.Scope{Repo: "NightsOut"}, "code-explorer", memory.Provenance{},
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
	if s := got.Memories[0].Status; s == nil || *s != schema.Unverified {
		t.Fatalf("status before delete = %v, want unverified", s)
	}

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
		t.Fatalf("default listing after delete: got %+v, want empty (invalidated excluded)", got2)
	}

	// include_invalidated=true still finds it, with its status and reason.
	includeInvalidated := true
	w3 := httptest.NewRecorder()
	h.ListMemories(w3, httptest.NewRequest(http.MethodGet, "/api/v1/memories?bucket="+bucket+"&include_invalidated=true", nil),
		schema.ListMemoriesParams{Bucket: &bucket, IncludeInvalidated: &includeInvalidated})
	var got3 schema.MemoryList
	if err := json.NewDecoder(w3.Body).Decode(&got3); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got3.Total != 1 || len(got3.Memories) != 1 {
		t.Fatalf("include_invalidated=true listing = %+v, want the one invalidated memory", got3)
	}
	m := got3.Memories[0]
	if m.Status == nil || *m.Status != schema.Invalidated {
		t.Fatalf("status after delete = %v, want invalidated", m.Status)
	}
	if m.InvalidationReason == nil || *m.InvalidationReason != "manual delete" {
		t.Fatalf("invalidation_reason = %v, want %q (default)", m.InvalidationReason, "manual delete")
	}

	// Deleting an already-invalidated memory is idempotent (soft-delete never
	// revives, and re-invalidating an existing point is a success, not a 404).
	delW2 := httptest.NewRecorder()
	h.DeleteMemory(delW2, httptest.NewRequest(http.MethodDelete, "/api/v1/memories/"+id, nil), id)
	if delW2.Code != http.StatusNoContent {
		t.Fatalf("re-delete status = %d, want 204 (idempotent)", delW2.Code)
	}
}

// TestDeleteMemory_CustomReason covers design doc §7 case 5's reason
// round-trip: a caller-supplied reason replaces the "manual delete" default.
func TestDeleteMemory_CustomReason(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)
	commitFact(t, h.taskMem, "NightsOut", "advice that turned out to be wrong")

	bucket := "repo:NightsOut"
	w := httptest.NewRecorder()
	h.ListMemories(w, httptest.NewRequest(http.MethodGet, "/api/v1/memories?bucket="+bucket, nil), schema.ListMemoriesParams{Bucket: &bucket})
	var got schema.MemoryList
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil || len(got.Memories) != 1 {
		t.Fatalf("seed listing = %+v (decode err %v), want exactly one entry", got, err)
	}
	id := got.Memories[0].Id

	body := strings.NewReader(`{"reason":"contradicted by a later, correct run"}`)
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/memories/"+id, body)
	delW := httptest.NewRecorder()
	h.DeleteMemory(delW, delReq, id)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delW.Code)
	}

	includeInvalidated := true
	w2 := httptest.NewRecorder()
	h.ListMemories(w2, httptest.NewRequest(http.MethodGet, "/api/v1/memories?bucket="+bucket+"&include_invalidated=true", nil),
		schema.ListMemoriesParams{Bucket: &bucket, IncludeInvalidated: &includeInvalidated})
	var got2 schema.MemoryList
	if err := json.NewDecoder(w2.Body).Decode(&got2); err != nil || len(got2.Memories) != 1 {
		t.Fatalf("include_invalidated listing = %+v (decode err %v), want exactly one entry", got2, err)
	}
	if r := got2.Memories[0].InvalidationReason; r == nil || *r != "contradicted by a later, correct run" {
		t.Fatalf("invalidation_reason = %v, want the custom reason", r)
	}
}

// With both taskMem and userMem configured, a listing merges entries from
// both and orders the COMBINED set by timestamp - not by concatenating one
// store's page after the other's. The four facts below interleave stores
// (task, user, task, user) with real time gaps between commits, so a naive
// concatenation (which would group all of one store's entries before the
// other's) produces a visibly different order than a correct merge sort.
func TestListMemories_MergesAndOrdersAcrossBothStores(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)
	h.userMem = newTestMemStore(t)

	const bucket = "NightsOut"
	commitFact(t, h.taskMem, bucket, "fact A (oldest, task)")
	time.Sleep(1100 * time.Millisecond) // RFC3339 timestamps are second-resolution
	commitFact(t, h.userMem, bucket, "fact B (user)")
	time.Sleep(1100 * time.Millisecond)
	commitFact(t, h.taskMem, bucket, "fact C (task)")
	time.Sleep(1100 * time.Millisecond)
	commitFact(t, h.userMem, bucket, "fact D (newest, user)")

	b := "repo:" + bucket
	w := httptest.NewRecorder()
	h.ListMemories(w, httptest.NewRequest(http.MethodGet, "/api/v1/memories?bucket="+b, nil), schema.ListMemoriesParams{Bucket: &b})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got schema.MemoryList
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 4 || len(got.Memories) != 4 {
		t.Fatalf("got %d/%d entries, want 4/4", len(got.Memories), got.Total)
	}
	wantOrder := []string{"fact D (newest, user)", "fact C (task)", "fact B (user)", "fact A (oldest, task)"}
	if gotOrder := memoryContents(got.Memories); strings.Join(gotOrder, "|") != strings.Join(wantOrder, "|") {
		t.Fatalf("merged order = %v, want %v (newest first across both stores)", gotOrder, wantOrder)
	}

	// Paging across the merged set: page 1 ends on fact C (task's) and page 2
	// starts on fact B (user's) - the boundary sits between the two stores'
	// entries, exactly where a store-by-store concatenation would misbehave.
	limit := 2
	w1 := httptest.NewRecorder()
	h.ListMemories(w1, httptest.NewRequest(http.MethodGet, "/api/v1/memories", nil), schema.ListMemoriesParams{Bucket: &b, Limit: &limit})
	var page1 schema.MemoryList
	if err := json.NewDecoder(w1.Body).Decode(&page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if page1.NextPageToken == nil || *page1.NextPageToken == "" {
		t.Fatalf("page1 carried no next_page_token, but a second page exists")
	}

	w2 := httptest.NewRecorder()
	h.ListMemories(w2, httptest.NewRequest(http.MethodGet, "/api/v1/memories", nil), schema.ListMemoriesParams{Bucket: &b, Limit: &limit, PageToken: page1.NextPageToken})
	var page2 schema.MemoryList
	if err := json.NewDecoder(w2.Body).Decode(&page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	if page2.NextPageToken != nil {
		t.Fatalf("page2 carried a next_page_token = %q, want none - it's the last page", *page2.NextPageToken)
	}

	if got := memoryContents(page1.Memories); strings.Join(got, "|") != "fact D (newest, user)|fact C (task)" {
		t.Fatalf("page1 = %v, want [D, C]", got)
	}
	if got := memoryContents(page2.Memories); strings.Join(got, "|") != "fact B (user)|fact A (oldest, task)" {
		t.Fatalf("page2 = %v, want [B, A]", got)
	}
	seen := map[string]bool{}
	for _, m := range append(append([]schema.Memory{}, page1.Memories...), page2.Memories...) {
		if seen[m.Id] {
			t.Fatalf("id %s appeared on both pages - duplicated across the store boundary", m.Id)
		}
		seen[m.Id] = true
	}
	if len(seen) != 4 {
		t.Fatalf("saw %d distinct ids across both pages, want 4 - an entry was dropped at the store boundary", len(seen))
	}
}

// TestListMemories_InvalidPageToken400: a page_token the store can't decode
// is a client error, not a 500 - mirrors TestListChats_InvalidPageToken400.
func TestListMemories_InvalidPageToken400(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)
	bad := "not-a-valid-token!!"
	w := httptest.NewRecorder()
	h.ListMemories(w, httptest.NewRequest(http.MethodGet, "/api/v1/memories", nil), schema.ListMemoriesParams{PageToken: &bad})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestListMemories_PageTokenBucketMismatch400: a token minted under one
// bucket filter must not be honored against a different one - it's issued
// scoped to the filter, same as store.chatsPageToken's scope binding.
func TestListMemories_PageTokenBucketMismatch400(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)
	for i := 0; i < 3; i++ {
		commitFact(t, h.taskMem, "NightsOut", fmt.Sprintf("fact %d", i))
	}

	bucketA := "repo:NightsOut"
	limit := 1
	w := httptest.NewRecorder()
	h.ListMemories(w, httptest.NewRequest(http.MethodGet, "/api/v1/memories", nil), schema.ListMemoriesParams{Bucket: &bucketA, Limit: &limit})
	var page1 schema.MemoryList
	if err := json.NewDecoder(w.Body).Decode(&page1); err != nil || page1.NextPageToken == nil {
		t.Fatalf("seed page1 = %+v (decode err %v), want a next_page_token", page1, err)
	}

	bucketB := "repo:OtherRepo"
	w2 := httptest.NewRecorder()
	h.ListMemories(w2, httptest.NewRequest(http.MethodGet, "/api/v1/memories", nil), schema.ListMemoriesParams{Bucket: &bucketB, PageToken: page1.NextPageToken})
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (token issued for a different bucket); body=%s", w2.Code, w2.Body.String())
	}
}

// A memory living only in the second configured store (userMem) must still
// be found and invalidated - invalidateMemory can't stop at the first store
// that doesn't have the id.
func TestDeleteMemory_FindsIDInSecondStore(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t) // stays empty - the id is only in userMem
	h.userMem = newTestMemStore(t)

	commitFact(t, h.userMem, "NightsOut", "only in the user store")

	bucket := "repo:NightsOut"
	w := httptest.NewRecorder()
	h.ListMemories(w, httptest.NewRequest(http.MethodGet, "/api/v1/memories?bucket="+bucket, nil), schema.ListMemoriesParams{Bucket: &bucket})
	var got schema.MemoryList
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil || len(got.Memories) != 1 {
		t.Fatalf("seed listing = %+v (decode err %v), want exactly one entry", got, err)
	}
	id := got.Memories[0].Id

	delW := httptest.NewRecorder()
	h.DeleteMemory(delW, httptest.NewRequest(http.MethodDelete, "/api/v1/memories/"+id, nil), id)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (id lives in the second configured store)", delW.Code)
	}

	w2 := httptest.NewRecorder()
	h.ListMemories(w2, httptest.NewRequest(http.MethodGet, "/api/v1/memories?bucket="+bucket, nil), schema.ListMemoriesParams{Bucket: &bucket})
	var got2 schema.MemoryList
	if err := json.NewDecoder(w2.Body).Decode(&got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got2.Memories) != 0 {
		t.Fatalf("after delete: %+v, want empty", got2)
	}
}

// An id in neither configured store is a 404, not a silent success or a
// crash from exhausting the store list.
func TestDeleteMemory_UnknownID_404WithBothStoresConfigured(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)
	h.userMem = newTestMemStore(t)

	w := httptest.NewRecorder()
	h.DeleteMemory(w, httptest.NewRequest(http.MethodDelete, "/api/v1/memories/does-not-exist", nil), "does-not-exist")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	adkmemory "google.golang.org/adk/memory"
)

// fakeEmbedder returns a fixed-dimension vector for every input, so tests are
// deterministic and don't need a real embedding server.
type fakeEmbedder struct{ dim int }

func (e fakeEmbedder) Dim() int { return e.dim }

func (e fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, e.dim)
		for j := range v {
			v[j] = float32(j) + 1
		}
		out[i] = v
	}
	return out, nil
}

// fakeQdrant is an in-memory stand-in for Qdrant's REST API. It records upserts
// and the last search request so tests can assert payloads and filters.
type fakeQdrant struct {
	mu          sync.Mutex
	collectDim  int  // dimension to report on GET; 0 ⇒ 404 (not created)
	created     bool // a PUT collection happened
	createdDim  int
	upserts     []qdrantPoint
	lastFilter  map[string]any
	searchReply []hit
}

func (f *fakeQdrant) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/collections/quack_memory", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if f.collectDim == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			resp := map[string]any{"result": map[string]any{"config": map[string]any{
				"params": map[string]any{"vectors": map[string]any{"size": f.collectDim}},
			}}}
			_ = json.NewEncoder(w).Encode(resp)
		case http.MethodPut:
			var body struct {
				Vectors struct {
					Size int `json:"size"`
				} `json:"vectors"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.created = true
			f.createdDim = body.Vectors.Size
			f.collectDim = body.Vectors.Size
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("/collections/quack_memory/points", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			Points []qdrantPoint `json:"points"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.upserts = append(f.upserts, body.Points...)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/collections/quack_memory/points/search", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if flt, ok := body["filter"].(map[string]any); ok {
			f.lastFilter = flt
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": f.searchReply})
	})
	return mux
}

func newTestService(t *testing.T, fq *fakeQdrant, dim int) *Service {
	t.Helper()
	srv := httptest.NewServer(fq.handler())
	t.Cleanup(srv.Close)
	s, err := New(context.Background(), fakeEmbedder{dim: dim}, srv.URL, "", "quack_memory")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestEnsureCollectionCreates(t *testing.T) {
	fq := &fakeQdrant{collectDim: 0} // not created yet
	newTestService(t, fq, 4)
	if !fq.created || fq.createdDim != 4 {
		t.Fatalf("created=%v dim=%d, want created with dim 4", fq.created, fq.createdDim)
	}
}

func TestEnsureCollectionDimMismatch(t *testing.T) {
	fq := &fakeQdrant{collectDim: 8} // exists with wrong size
	srv := httptest.NewServer(fq.handler())
	t.Cleanup(srv.Close)
	_, err := New(context.Background(), fakeEmbedder{dim: 4}, srv.URL, "", "quack_memory")
	if err == nil || !strings.Contains(err.Error(), "vector size") {
		t.Fatalf("err = %v, want a vector-size mismatch error", err)
	}
}

func TestCommitWritesPayloadAndDeterministicID(t *testing.T) {
	fq := &fakeQdrant{collectDim: 4}
	s := newTestService(t, fq, 4)

	req := CommitRequest{
		AppName: "web-researcher", UserID: "u", Agent: "web-researcher",
		Query: "best time to visit dublin", Finding: "May–September.",
		Sources: []string{"https://a.example", "https://b.example"}, Score: 0.88,
	}
	if err := s.Commit(context.Background(), req); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Re-commit the same (appName,userID,query) → must upsert in place (same ID).
	req.Finding = "Revised: late spring to early autumn."
	if err := s.Commit(context.Background(), req); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}

	if len(fq.upserts) != 2 {
		t.Fatalf("upserts = %d, want 2", len(fq.upserts))
	}
	p := fq.upserts[0]
	if len(p.Vector) != 4 {
		t.Errorf("vector len = %d, want 4", len(p.Vector))
	}
	if p.Payload[fieldFinding] != "May–September." {
		t.Errorf("finding payload = %v", p.Payload[fieldFinding])
	}
	if p.Payload[fieldAppName] != "web-researcher" || p.Payload[fieldUserID] != "u" {
		t.Errorf("namespace payload = %v/%v", p.Payload[fieldAppName], p.Payload[fieldUserID])
	}
	if p.Payload[fieldQuery] != "best time to visit dublin" {
		t.Errorf("query payload = %v", p.Payload[fieldQuery])
	}
	if fq.upserts[0].ID != fq.upserts[1].ID {
		t.Errorf("IDs differ across same-query commits: %q vs %q", fq.upserts[0].ID, fq.upserts[1].ID)
	}
	if fq.upserts[0].ID != pointID("web-researcher", "u", "best time to visit dublin") {
		t.Errorf("ID = %q, not the expected deterministic value", fq.upserts[0].ID)
	}
}

func TestSearchMemoryMapsHitsAndScopes(t *testing.T) {
	fq := &fakeQdrant{collectDim: 4, searchReply: []hit{{
		ID:    "abc",
		Score: 0.9,
		Payload: map[string]any{
			fieldFinding:   "Dublin is best May–September.",
			fieldAgent:     "web-researcher",
			fieldTimestamp: "2026-06-12T00:00:00Z",
		},
	}}}
	s := newTestService(t, fq, 4)

	resp, err := s.SearchMemory(context.Background(), &adkmemory.SearchRequest{
		Query: "when to visit dublin", AppName: "web-researcher", UserID: "u",
	})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("memories = %d, want 1", len(resp.Memories))
	}
	m := resp.Memories[0]
	if m.Content == nil || len(m.Content.Parts) == 0 || m.Content.Parts[0].Text != "Dublin is best May–September." {
		t.Errorf("mapped content = %+v", m.Content)
	}
	if m.Author != "web-researcher" {
		t.Errorf("author = %q", m.Author)
	}
	// The search must be scoped by app_name + user_id.
	must, _ := fq.lastFilter["must"].([]any)
	if len(must) != 2 {
		t.Fatalf("filter must = %v, want 2 clauses", fq.lastFilter)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	fq := &fakeQdrant{collectDim: 4}
	s := newTestService(t, fq, 4)
	resp, err := s.SearchMemory(context.Background(), &adkmemory.SearchRequest{Query: "  "})
	if err != nil || len(resp.Memories) != 0 {
		t.Fatalf("empty query: resp=%+v err=%v", resp, err)
	}
}

func TestAddSessionToMemoryIsNoop(t *testing.T) {
	fq := &fakeQdrant{collectDim: 4}
	s := newTestService(t, fq, 4)
	if err := s.AddSessionToMemory(context.Background(), nil); err != nil {
		t.Fatalf("AddSessionToMemory should be a no-op, got %v", err)
	}
	if len(fq.upserts) != 0 {
		t.Errorf("AddSessionToMemory wrote %d points, want 0", len(fq.upserts))
	}
}

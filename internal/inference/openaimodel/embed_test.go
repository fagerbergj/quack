package openaimodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeEmbeddings serves an OpenAI-compatible /embeddings response. It returns the
// data out of input order (indices reversed) so the test proves Embed re-orders
// by the declared index rather than trusting array position.
func fakeEmbeddings(t *testing.T, vectors [][]float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body.Input) != len(vectors) {
			http.Error(w, "input count mismatch", http.StatusBadRequest)
			return
		}
		type emb struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		data := make([]emb, 0, len(vectors))
		for i := len(vectors) - 1; i >= 0; i-- { // reversed on purpose
			data = append(data, emb{Object: "embedding", Index: i, Embedding: vectors[i]})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"model":  "test-embed",
			"data":   data,
			"usage":  map[string]int{"prompt_tokens": 1, "total_tokens": 1},
		})
	}))
}

func TestEmbed_MapsAndOrders(t *testing.T) {
	want := [][]float64{{1, 2, 3}, {4, 5, 6}}
	srv := fakeEmbeddings(t, want)
	defer srv.Close()

	m := NewOpenAIModel("test-embed", srv.URL, "key")
	got, err := m.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d vectors, want 2", len(got))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("vector %d length %d, want %d", i, len(got[i]), len(want[i]))
		}
		for j := range want[i] {
			if got[i][j] != float32(want[i][j]) {
				t.Fatalf("vector[%d][%d] = %v, want %v (ordering by index failed?)", i, j, got[i][j], want[i][j])
			}
		}
	}
}

func TestEmbed_Empty(t *testing.T) {
	m := NewOpenAIModel("test-embed", "http://invalid.invalid", "key")
	got, err := m.Embed(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("Embed(nil) = (%v, %v), want (nil, nil) with no request made", got, err)
	}
}

// TestEmbedWithUsage_PropagatesTokenCounts pins the root cause this method
// fixes: Embed alone discarded the response's usage block entirely.
func TestEmbedWithUsage_PropagatesTokenCounts(t *testing.T) {
	srv := fakeEmbeddings(t, [][]float64{{1, 2, 3}, {4, 5, 6}})
	defer srv.Close()

	m := NewOpenAIModel("test-embed", srv.URL, "key")
	_, usage, err := m.EmbedWithUsage(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedWithUsage: %v", err)
	}
	if usage.PromptTokens != 1 || usage.TotalTokens != 1 {
		t.Errorf("usage = %+v, want PromptTokens=1 TotalTokens=1 (from the fake server's usage block)", usage)
	}
}

// TestEmbedWithUsage_Empty mirrors TestEmbed_Empty for the usage-returning method.
func TestEmbedWithUsage_Empty(t *testing.T) {
	m := NewOpenAIModel("test-embed", "http://invalid.invalid", "key")
	got, usage, err := m.EmbedWithUsage(context.Background(), nil)
	if err != nil || got != nil || usage != (EmbedUsage{}) {
		t.Fatalf("EmbedWithUsage(nil) = (%v, %+v, %v), want (nil, zero, nil) with no request made", got, usage, err)
	}
}

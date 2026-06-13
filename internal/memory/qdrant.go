package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// qdrantClient is a minimal Qdrant REST client: just the three operations
// memory needs (ensure collection, upsert one point, vector search). It hits
// the HTTP API directly so we avoid pulling in the heavy gRPC go-client module.
type qdrantClient struct {
	base       string
	apiKey     string
	collection string
	http       *http.Client
}

func newQdrantClient(baseURL, apiKey, collection string) *qdrantClient {
	return &qdrantClient{
		base:       strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		collection: collection,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

// qdrantPoint is one stored memory: a vector plus its provenance payload.
type qdrantPoint struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// EnsureCollection creates the collection sized for dim vectors (cosine
// distance) if it does not exist. If it already exists with a different vector
// size it returns an error rather than silently recreating — a changed
// embedding model means a reindex, not a destructive auto-migration.
func (q *qdrantClient) EnsureCollection(ctx context.Context, dim int) error {
	status, respBody, err := q.do(ctx, http.MethodGet, "/collections/"+q.collection, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		var existing struct {
			Result struct {
				Config struct {
					Params struct {
						Vectors struct {
							Size int `json:"size"`
						} `json:"vectors"`
					} `json:"params"`
				} `json:"config"`
			} `json:"result"`
		}
		if err := json.Unmarshal(respBody, &existing); err != nil {
			return fmt.Errorf("qdrant: decode collection %q: %w", q.collection, err)
		}
		if got := existing.Result.Config.Params.Vectors.Size; got != dim {
			return fmt.Errorf("qdrant: collection %q has vector size %d but embedder produces %d; recreate the collection or revert the embedding model", q.collection, got, dim)
		}
		return nil
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("qdrant: get collection %q: status %d: %s", q.collection, status, respBody)
	}

	body := map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	}
	status, respBody, err = q.do(ctx, http.MethodPut, "/collections/"+q.collection, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("qdrant: create collection %q: status %d: %s", q.collection, status, respBody)
	}
	return nil
}

// Upsert writes one point, waiting for it to be indexed so a recall immediately
// after a commit can see it.
func (q *qdrantClient) Upsert(ctx context.Context, p qdrantPoint) error {
	body := map[string]any{"points": []qdrantPoint{p}}
	status, respBody, err := q.do(ctx, http.MethodPut, "/collections/"+q.collection+"/points?wait=true", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("qdrant: upsert: status %d: %s", status, respBody)
	}
	return nil
}

// hit is one search result with its payload.
type hit struct {
	ID      string         `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

// Search returns up to limit nearest points whose payload matches appName +
// userID (the per-agent, per-user memory namespace).
func (q *qdrantClient) Search(ctx context.Context, vector []float32, appName, userID string, limit int) ([]hit, error) {
	body := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
		"filter": map[string]any{
			"must": []map[string]any{
				{"key": fieldAppName, "match": map[string]any{"value": appName}},
				{"key": fieldUserID, "match": map[string]any{"value": userID}},
			},
		},
	}
	status, respBody, err := q.do(ctx, http.MethodPost, "/collections/"+q.collection+"/points/search", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("qdrant: search: status %d: %s", status, respBody)
	}
	var resp struct {
		Result []hit `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("qdrant: decode search: %w", err)
	}
	return resp.Result, nil
}

// do issues a request and returns the status code and raw response body. It
// returns an error only for transport/marshal failures, not for HTTP error
// statuses — callers branch on the status (e.g. 404 vs 200) and use the body
// for error context.
func (q *qdrantClient) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("qdrant: marshal %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.base+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("qdrant: new request %s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if q.apiKey != "" {
		req.Header.Set("api-key", q.apiKey)
	}
	resp, err := q.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("qdrant: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("qdrant: read %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

// pointID derives a deterministic UUID (Qdrant accepts UUID-string IDs) from the
// memory's namespace and dedup key, so re-committing the same finding (judge
// revise loops, retries, re-asking the same question) upserts in place instead
// of accumulating duplicates.
func pointID(appName, userID, key string) string {
	sum := sha256.Sum256([]byte(appName + "\x00" + userID + "\x00" + normalize(key)))
	h := sum[:16]
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// normalize lower-cases and collapses whitespace so trivially different phrasings
// of the same dedup key map to the same point.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

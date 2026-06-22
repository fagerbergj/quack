package docstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FTSIndex is the full-text index over document records (keyword search). The
// reference adapter is OpenSearch; like the record store it's selected by a
// `kind` factory and swappable. Indexing is derived from the document record, so
// the index holds a denormalised copy (title/summary/content/tags) — search
// returns lightweight hits the agent can then load_document for the full record.
type FTSIndex interface {
	Index(ctx context.Context, d Document) error
	Search(ctx context.Context, query string, size int) ([]FTSHit, error)
}

// FTSHit is one search result: enough for the agent to decide what to open.
type FTSHit struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// NewFTS selects the full-text index adapter for kind (default: opensearch).
func NewFTS(kind, addr, index string) (FTSIndex, error) {
	if kind == "" {
		kind = "opensearch"
	}
	switch kind {
	case "opensearch":
		if addr == "" {
			return nil, fmt.Errorf("docstore: opensearch url is empty")
		}
		if index == "" {
			index = "documents"
		}
		return &openSearch{
			client: &http.Client{Timeout: 15 * time.Second},
			base:   strings.TrimRight(addr, "/"),
			index:  index,
		}, nil
	default:
		return nil, fmt.Errorf("docstore: unsupported fts kind %q", kind)
	}
}

// openSearch is the OpenSearch adapter, talking to a keyless internal instance
// over plain HTTP (same trust model as the searxng/crawl4ai backends).
type openSearch struct {
	client *http.Client
	base   string
	index  string
}

// Index upserts the document by id. refresh=true makes it immediately searchable
// (fine for this workload; documents are ingested one at a time, not bulk-loaded).
func (o *openSearch) Index(ctx context.Context, d Document) error {
	body, err := json.Marshal(map[string]any{
		"title": d.Title, "summary": d.Summary, "content": d.Content,
		"tags": d.Tags, "series": d.Series, "date_month": d.DateMonth,
	})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", o.base, o.index, url.PathEscape(d.ID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("opensearch: build index request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("opensearch: index request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("opensearch: index returned %s", resp.Status)
	}
	return nil
}

func (o *openSearch) Search(ctx context.Context, query string, size int) ([]FTSHit, error) {
	if size <= 0 {
		size = 10
	}
	body, err := json.Marshal(map[string]any{
		"size": size,
		"query": map[string]any{
			"multi_match": map[string]any{
				"query":  query,
				"fields": []string{"title^2", "summary", "content", "tags"},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	endpoint := o.base + "/" + o.index + "/_search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("opensearch: build search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opensearch: search request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // index not created yet ⇒ no documents indexed ⇒ no hits
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opensearch: search returned %s", resp.Status)
	}
	var parsed struct {
		Hits struct {
			Hits []struct {
				ID     string `json:"_id"`
				Source struct {
					Title   string `json:"title"`
					Summary string `json:"summary"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("opensearch: decode search: %w", err)
	}
	hits := make([]FTSHit, 0, len(parsed.Hits.Hits))
	for _, h := range parsed.Hits.Hits {
		hits = append(hits, FTSHit{ID: h.ID, Title: h.Source.Title, Summary: h.Source.Summary})
	}
	return hits, nil
}

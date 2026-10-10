package langfuse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// DatasetItem is the part of a Langfuse dataset item quack reads.
type DatasetItem struct {
	ID    string `json:"id"`
	Input any    `json:"input"`
}

type CreateDatasetItemRequest struct {
	DatasetName    string `json:"datasetName"`
	ID             string `json:"id,omitempty"`
	Input          any    `json:"input,omitempty"`
	ExpectedOutput any    `json:"expectedOutput,omitempty"`
	Metadata       any    `json:"metadata,omitempty"`
}

type CreateDatasetRunItemRequest struct {
	DatasetItemID  string `json:"datasetItemId"`
	RunName        string `json:"runName"`
	TraceID        string `json:"traceId,omitempty"`
	RunDescription string `json:"runDescription,omitempty"`
}

// DatasetExists reports whether the named dataset exists (false on 404).
func (c *Client) DatasetExists(ctx context.Context, name string) (bool, error) {
	status, err := c.call(ctx, http.MethodGet, "/api/public/v2/datasets/"+url.PathEscape(name), nil, nil, nil)
	if status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) CreateDataset(ctx context.Context, name string) error {
	_, err := c.call(ctx, http.MethodPost, "/api/public/v2/datasets", nil, map[string]string{"name": name}, nil)
	return err
}

// ListDatasetItems returns one 1-based page of dataset's items and the total page count.
func (c *Client) ListDatasetItems(ctx context.Context, dataset string, page int) ([]DatasetItem, int, error) {
	q := url.Values{"datasetName": {dataset}, "page": {strconv.Itoa(page)}}
	var out struct {
		Data []DatasetItem `json:"data"`
		Meta struct {
			TotalPages int `json:"totalPages"`
		} `json:"meta"`
	}
	_, err := c.call(ctx, http.MethodGet, "/api/public/dataset-items", q, nil, &out)
	return out.Data, out.Meta.TotalPages, err
}

func (c *Client) CreateDatasetItem(ctx context.Context, req CreateDatasetItemRequest) error {
	_, err := c.call(ctx, http.MethodPost, "/api/public/dataset-items", nil, req, nil)
	return err
}

func (c *Client) CreateDatasetRunItem(ctx context.Context, req CreateDatasetRunItemRequest) error {
	_, err := c.call(ctx, http.MethodPost, "/api/public/dataset-run-items", nil, req, nil)
	return err
}

// call sends one request, maps non-2xx to *APIError, and decodes a 2xx body into out when non-nil.
func (c *Client) call(ctx context.Context, method, path string, q url.Values, body, out any) (int, error) {
	resp, err := c.do(ctx, method, path, q, body)
	if err != nil {
		return 0, err
	}
	defer func() { drain(resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("langfuse: %s %s: %w", method, path, newAPIError(resp))
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxOKBody)).Decode(out); err != nil {
			return resp.StatusCode, markPermanent(fmt.Errorf("langfuse: decode %s: %w", path, err))
		}
	}
	return resp.StatusCode, nil
}

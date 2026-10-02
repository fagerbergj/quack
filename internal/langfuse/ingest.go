package langfuse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// IngestEvent is one ingestion batch event (type "trace-create", "score-create", ...);
// ID must be unique per event.
type IngestEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Body      any    `json:"body"`
}

// Ingest posts events to /api/public/ingestion, the only write path for traces and
// scores. The API answers 207 with per-event failures, which are returned as an error.
func (c *Client) Ingest(ctx context.Context, events []IngestEvent) error {
	resp, err := c.do(ctx, http.MethodPost, "/api/public/ingestion", nil, map[string]any{"batch": events})
	if err != nil {
		return err
	}
	defer func() { drain(resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("langfuse: ingest: %w", newAPIError(resp))
	}
	var out struct {
		Errors []struct {
			ID      string `json:"id"`
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOKBody)).Decode(&out); err != nil {
		return markPermanent(fmt.Errorf("langfuse: decode ingest response: %w", err))
	}
	if len(out.Errors) > 0 {
		e := out.Errors[0]
		return fmt.Errorf("langfuse: ingest: %d of %d events failed; first %s: status %d %s", len(out.Errors), len(events), e.ID, e.Status, e.Message)
	}
	return nil
}

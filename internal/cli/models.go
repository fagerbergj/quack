package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/httpx"
)

// ListModels queries {endpoint}/models and returns the model IDs, sorted. Works
// against any OpenAI-compatible server (the response is `{ "data": [{ "id": ... }] }`).
// The wizard uses this to populate role selects; on failure the caller falls
// back to manual entry.
func ListModels(ctx context.Context, endpoint, apiKey string) ([]string, error) {
	ep := strings.TrimRight(endpoint, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: httpx.NewTransport(nil)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact %s: %w", ep, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", ep, resp.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("parse /models: %w", err)
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

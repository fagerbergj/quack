package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// searxngSearcher: SearXNG is a trusted internal host, so it uses the plain, unguarded client.
type searxngSearcher struct {
	client *http.Client
	base   string // trimmed of a trailing slash
}

func (s *searxngSearcher) Search(ctx context.Context, query string) ([]SearchResult, string, error) {
	return searchWeb(ctx, s.client, s.base, query)
}

type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
	// UnresponsiveEngines: [engine, reason, ...] tuples. SearXNG returns 200 when its engines fail, so this is
	// the only signal telling "rate-limited" from "no matches".
	UnresponsiveEngines [][]any `json:"unresponsive_engines"`
}

// searchWeb: the string is a non-fatal note (some engines rate-limited); an error means nothing usable.
func searchWeb(ctx context.Context, client *http.Client, base, query string) ([]SearchResult, string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, "", fmt.Errorf("web_search: empty query")
	}
	endpoint := base + "/search?" + url.Values{"q": {q}, "format": {"json"}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("web_search: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("web_search: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("web_search: backend returned %s", resp.Status)
	}

	var parsed searxResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, "", fmt.Errorf("web_search: decode response: %w", err)
	}

	results := make([]SearchResult, 0, maxSearchResults)
	for _, r := range parsed.Results {
		if len(results) >= maxSearchResults {
			break
		}
		results = append(results, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}

	// Surface engine failures so the agent can tell "rate-limited" from "no matches".
	if down := formatUnresponsiveEngines(parsed.UnresponsiveEngines); down != "" {
		if len(results) == 0 {
			return nil, "", fmt.Errorf("web_search: no results - every search backend failed: %s (likely rate-limited; back off and retry shortly)", down)
		}
		return results, fmt.Sprintf("partial results: some search backends failed: %s", down), nil
	}

	return results, "", nil
}

// formatUnresponsiveEngines renders "engine (reason), ..."; "" when none failed.
func formatUnresponsiveEngines(engines [][]any) string {
	parts := make([]string, 0, len(engines))
	for _, e := range engines {
		var name, reason string
		if len(e) > 0 {
			name, _ = e[0].(string)
		}
		if len(e) > 1 {
			reason, _ = e[1].(string)
		}
		switch {
		case name != "" && reason != "":
			parts = append(parts, fmt.Sprintf("%s (%s)", name, reason))
		case name != "":
			parts = append(parts, name)
		}
	}
	return strings.Join(parts, ", ")
}

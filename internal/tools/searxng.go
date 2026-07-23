package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// searxngSearcher is the SearXNG adapter for the WebSearcher port. SearXNG is a
// trusted internal host, so it uses the plain client (not the SSRF-guarded one).
type searxngSearcher struct {
	client *http.Client
	base   string // trimmed of a trailing slash
}

func (s *searxngSearcher) Search(ctx context.Context, query string) ([]SearchResult, string, error) {
	return searchWeb(ctx, s.client, s.base, query)
}

// searxResponse is the subset of SearXNG's JSON we consume.
type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
	// UnresponsiveEngines lists backends SearXNG could not get results from for
	// this query, each an [engine, reason, ...] tuple (e.g. ["brave", "Too many
	// requests"]). SearXNG still returns HTTP 200 when its upstream engines fail,
	// so this is the ONLY signal distinguishing "rate-limited" from "no matches".
	UnresponsiveEngines [][]any `json:"unresponsive_engines"`
}

// searchWeb queries SearXNG's JSON API and returns the top results. The second
// return value is a non-fatal note for the agent (e.g. some backends were
// rate-limited but others returned hits); an error is reserved for a search that
// produced nothing usable.
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
	defer resp.Body.Close()
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

	// SearXNG returns HTTP 200 even when its upstream engines fail (rate limits,
	// timeouts), reporting them only in unresponsive_engines. Surface that so the
	// agent can tell "rate-limited" apart from "genuinely no matches" instead of
	// seeing a silently empty list.
	if down := formatUnresponsiveEngines(parsed.UnresponsiveEngines); down != "" {
		if len(results) == 0 {
			return nil, "", fmt.Errorf("web_search: no results - every search backend failed: %s (likely rate-limited; back off and retry shortly)", down)
		}
		return results, fmt.Sprintf("partial results: some search backends failed: %s", down), nil
	}

	return results, "", nil
}

// formatUnresponsiveEngines renders SearXNG's unresponsive_engines entries (each
// an [engine, reason, ...] tuple) as "engine (reason), ...". Returns "" when no
// engine failed.
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

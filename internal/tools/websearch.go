package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// maxSearchResults caps how many hits web_search returns to keep the agent's
// context small.
const maxSearchResults = 8

type searchArgs struct {
	Query string `json:"query"`
}

// SearchResult is one search hit returned to the agent.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// searchResponse wraps the hits in an object. ADK requires a tool's result to
// convert to a map (it lands in FunctionResponse.Response, a map[string]any), so
// a top-level array fails — we return {results: [...]} instead.
type searchResponse struct {
	Results []SearchResult `json:"results"`
}

// searxResponse is the subset of SearXNG's JSON we consume.
type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// newWebSearch builds the web_search tool over a SearXNG backend. SearXNG is a
// trusted internal host, so it uses the plain client (not the SSRF-guarded one).
func newWebSearch(d Deps) (tool.Tool, error) {
	if d.SearXNG == "" {
		return nil, fmt.Errorf("web_search requires a SearXNG backend URL")
	}
	base := strings.TrimRight(d.SearXNG, "/")
	client := d.Client

	return functiontool.New[searchArgs, searchResponse](
		functiontool.Config{
			Name:        "web_search",
			Description: "Search the web for a query. Returns {results: [{title, url, snippet}]}. Use the urls with the fetch tool to read a page.",
		},
		func(tc tool.Context, a searchArgs) (searchResponse, error) {
			results, err := searchWeb(tc, client, base, a.Query)
			return searchResponse{Results: results}, err
		},
	)
}

// searchWeb queries SearXNG's JSON API and returns the top results.
func searchWeb(ctx context.Context, client *http.Client, base, query string) ([]SearchResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("web_search: empty query")
	}
	endpoint := base + "/search?" + url.Values{"q": {q}, "format": {"json"}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("web_search: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web_search: backend returned %s", resp.Status)
	}

	var parsed searxResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("web_search: decode response: %w", err)
	}

	results := make([]SearchResult, 0, maxSearchResults)
	for _, r := range parsed.Results {
		if len(results) >= maxSearchResults {
			break
		}
		results = append(results, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return results, nil
}

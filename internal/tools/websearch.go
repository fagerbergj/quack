package tools

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// maxSearchResults: caps web_search hits per query to keep agent context small.
const maxSearchResults = 8

// maxBatchQueries bounds one web_search call's query count (sequential backend calls per call).
const maxBatchQueries = 20

type searchArgs struct {
	Queries []string `json:"queries"`
}

// SearchResult: one search hit.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// queryResult: one query's hits within a batched web_search call.
type queryResult struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
	Note    string         `json:"note,omitempty"`
}

// searchResponse: wraps hits in an object (ADK requires map result, not array).
type searchResponse struct {
	Queries []queryResult `json:"queries"`
}

func newWebSearch(d Deps) (tool.Tool, error) {
	searcher, err := newWebSearcher(d.WebSearch.Kind, d.WebSearch.URL, d.WebSearch.Key, d.Client)
	if err != nil {
		return nil, err
	}
	return functiontool.New[searchArgs, searchResponse](
		functiontool.Config{
			Name: "web_search",
			Description: fmt.Sprintf("Search the web for a batch of queries. Takes `queries: [\"q1\", \"q2\", ...]` "+
				"(a single query is still a one-element list; up to %d per call) and runs them all in one call - "+
				"plan your searches up front rather than issuing them one at a time. Returns {queries: [{query, "+
				"results: [{title, url, snippet}], note}]}, one group per query; a URL already returned for an "+
				"earlier query in the same call is not repeated. Use the urls with the fetch tool to read a page.", maxBatchQueries),
		},
		func(tc agent.Context, a searchArgs) (searchResponse, error) {
			if len(a.Queries) == 0 {
				return searchResponse{}, errors.New("web_search: queries must be non-empty")
			}
			if len(a.Queries) > maxBatchQueries {
				return searchResponse{}, fmt.Errorf("web_search: %d queries exceeds the %d-query batch limit; split into smaller batches", len(a.Queries), maxBatchQueries)
			}
			return runSearches(tc, searcher, a.Queries), nil
		},
	)
}

// runSearches: runs every query in order, deduplicating hits across queries
// by URL so a page shared between two queries' results shows up once.
func runSearches(tc agent.Context, searcher WebSearcher, queries []string) searchResponse {
	seen := make(map[string]bool)
	out := make([]queryResult, 0, len(queries))
	for _, q := range queries {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		results, note, err := searcher.Search(tc, q)
		if err != nil {
			out = append(out, queryResult{Query: q, Note: err.Error()})
			continue
		}
		out = append(out, queryResult{Query: q, Results: dedupByURL(results, seen), Note: note})
	}
	return searchResponse{Queries: out}
}

// dedupByURL: drops results whose URL another query in this batch already
// returned, mutating seen so a later query sees the union so far.
func dedupByURL(results []SearchResult, seen map[string]bool) []SearchResult {
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		if r.URL == "" || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		out = append(out, r)
	}
	return out
}

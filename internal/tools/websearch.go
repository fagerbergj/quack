package tools

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// maxSearchResults: caps web_search hits to keep agent context small.
const maxSearchResults = 8

type searchArgs struct {
	Query string `json:"query"`
}

// SearchResult: one search hit.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// searchResponse: wraps hits in an object (ADK requires map result, not array).
type searchResponse struct {
	Results []SearchResult `json:"results"`
	Note string `json:"note,omitempty"`
}

// newWebSearch: builds web_search over a config-selected backend.
func newWebSearch(d Deps) (tool.Tool, error) {
	searcher, err := newWebSearcher(d.WebSearch.Kind, d.WebSearch.URL, d.WebSearch.Key, d.Client)
	if err != nil {
		return nil, err
	}
	return functiontool.New[searchArgs, searchResponse](
		functiontool.Config{
			Name:        "web_search",
			Description: "Search the web for a query. Returns {results: [{title, url, snippet}]}. Use the urls with the fetch tool to read a page.",
		},
		func(tc agent.Context, a searchArgs) (searchResponse, error) {
			results, note, err := searcher.Search(tc, a.Query)
			return searchResponse{Results: results, Note: note}, err
		},
	)
}

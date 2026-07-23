package tools

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
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
// a top-level array fails - we return {results: [...]} instead.
type searchResponse struct {
	Results []SearchResult `json:"results"`
	// Note carries an out-of-band warning to the agent - e.g. that some search
	// backends were rate-limited, so coverage was reduced. Omitted when empty.
	Note string `json:"note,omitempty"`
}

// newWebSearch builds the web_search tool over a config-selected WebSearcher
// backend (SearXNG today). The backend is a port (see backends.go) so it can be
// swapped without touching this tool.
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

package tools

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// stubSearcher: WebSearcher double returning canned results/errors per query.
type stubSearcher struct {
	results map[string][]SearchResult
	notes   map[string]string
	errs    map[string]error
}

func (s stubSearcher) Search(_ context.Context, query string) ([]SearchResult, string, error) {
	if err, ok := s.errs[query]; ok {
		return nil, "", err
	}
	return s.results[query], s.notes[query], nil
}

func TestRunSearches(t *testing.T) {
	cases := []struct {
		name    string
		queries []string
		stub    stubSearcher
		want    []queryResult
	}{
		{
			name:    "single query is a one-element batch",
			queries: []string{"cats"},
			stub: stubSearcher{results: map[string][]SearchResult{
				"cats": {{Title: "Cats", URL: "https://a.com/cats", Snippet: "s"}},
			}},
			want: []queryResult{
				{Query: "cats", Results: []SearchResult{{Title: "Cats", URL: "https://a.com/cats", Snippet: "s"}}},
			},
		},
		{
			name:    "blank queries are skipped",
			queries: []string{"", "  ", "cats"},
			stub: stubSearcher{results: map[string][]SearchResult{
				"cats": {{Title: "Cats", URL: "https://a.com/cats"}},
			}},
			want: []queryResult{
				{Query: "cats", Results: []SearchResult{{Title: "Cats", URL: "https://a.com/cats"}}},
			},
		},
		{
			name:    "a URL shared by two queries is deduplicated to the first",
			queries: []string{"cats", "kittens"},
			stub: stubSearcher{results: map[string][]SearchResult{
				"cats":    {{Title: "Cats", URL: "https://a.com/x"}},
				"kittens": {{Title: "Kittens", URL: "https://a.com/x"}, {Title: "Other", URL: "https://b.com/y"}},
			}},
			want: []queryResult{
				{Query: "cats", Results: []SearchResult{{Title: "Cats", URL: "https://a.com/x"}}},
				{Query: "kittens", Results: []SearchResult{{Title: "Other", URL: "https://b.com/y"}}},
			},
		},
		{
			name:    "a per-query search error is reported as a note, not a batch failure",
			queries: []string{"cats", "dogs"},
			stub: stubSearcher{
				results: map[string][]SearchResult{"dogs": {{Title: "Dogs", URL: "https://a.com/d"}}},
				errs:    map[string]error{"cats": errors.New("backend unavailable")},
			},
			want: []queryResult{
				{Query: "cats", Note: "backend unavailable"},
				{Query: "dogs", Results: []SearchResult{{Title: "Dogs", URL: "https://a.com/d"}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runSearches(newFakeCtx(), tc.stub, tc.queries)
			if !reflect.DeepEqual(got.Queries, tc.want) {
				t.Errorf("runSearches() = %+v, want %+v", got.Queries, tc.want)
			}
		})
	}
}

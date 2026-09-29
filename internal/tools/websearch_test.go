package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
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
			got, err := runSearches(newFakeCtx(), tc.stub, tc.queries)
			if err != nil {
				t.Fatalf("runSearches() err = %v, want none while any query succeeds", err)
			}
			if !reflect.DeepEqual(got.Queries, tc.want) {
				t.Errorf("runSearches() = %+v, want %+v", got.Queries, tc.want)
			}
		})
	}
}

// TestRunSearches_AllFailedIsAClearError: empty result lists for every query
// invite URL guessing, so a total failure is an error telling the model so.
func TestRunSearches_AllFailedIsAClearError(t *testing.T) {
	down := errors.New("exa REST got 401 Unauthorized")
	_, err := runSearches(newFakeCtx(), stubSearcher{errs: map[string]error{"a": down, "b": down}}, []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "search unavailable") || !strings.Contains(err.Error(), "do not guess URLs") {
		t.Fatalf("err = %v, want a search-unavailable error steering away from guessed URLs", err)
	}
}

// TestFallbackSearcher: a failing backend falls through to the next configured
// one, and each failure streak logs one WARN, not one per query.
func TestFallbackSearcher(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	hit := []SearchResult{{Title: "T", URL: "https://a.com"}}
	primary := stubSearcher{errs: map[string]error{"q1": errors.New("401"), "q2": errors.New("401")}}
	chain := fallbackSearcher{
		{name: "exa-rest", s: primary},
		{name: "searxng", s: stubSearcher{results: map[string][]SearchResult{"q1": hit, "q2": hit}}},
	}
	for _, q := range []string{"q1", "q2"} {
		got, _, err := chain.Search(context.Background(), q)
		if err != nil || !reflect.DeepEqual(got, hit) {
			t.Fatalf("%s: got %v, %v - want the fallback's results", q, got, err)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("WARN lines = %d, want 1 per failure streak:\n%s", n, logs.String())
	}

	dead := fallbackSearcher{{name: "only", s: primary}}
	if _, _, err := dead.Search(context.Background(), "q1"); err == nil {
		t.Error("a chain whose every backend fails must return an error")
	}
}

// TestNewWebSearcher_FallbackChain: keyed Exa falls back to keyless Exa, then
// to the SearXNG url the same config names.
func TestNewWebSearcher_FallbackChain(t *testing.T) {
	names := func(s WebSearcher) []string {
		var out []string
		for _, b := range s.(fallbackSearcher) {
			out = append(out, b.name)
		}
		return out
	}
	cases := []struct {
		kind, url, key string
		want           []string
	}{
		{"exa", "", "k", []string{"exa-rest", "exa-keyless"}},
		{"exa", "http://searx/", "k", []string{"exa-rest", "exa-keyless", "searxng"}},
		{"exa", "", "", []string{"exa-keyless"}},
		{"searxng", "http://searx", "", []string{"searxng"}},
	}
	for _, c := range cases {
		s, err := newWebSearcher(c.kind, c.url, c.key, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(s); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s url=%q key=%q: chain = %v, want %v", c.kind, c.url, c.key, got, c.want)
		}
	}
}

// TestWebSearchTool_EmptyAndTooManyQueriesError pins nit 8 (empty queries
// must error like empty urls) and blocker 2's query-count cap.
func TestWebSearchTool_EmptyAndTooManyQueriesError(t *testing.T) {
	tl, err := newWebSearch(Deps{WebSearch: Backend{Kind: "searxng", URL: "http://x"}})
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatal("web_search tool is not runnable")
	}
	if _, err := rt.Run(newFakeCtx(), map[string]any{"queries": []string{}}); err == nil {
		t.Error("empty queries should error, same as empty urls")
	}
	tooMany := make([]string, maxBatchQueries+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("q%d", i)
	}
	if _, err := rt.Run(newFakeCtx(), map[string]any{"queries": tooMany}); err == nil {
		t.Error("a batch over maxBatchQueries should error")
	}
}

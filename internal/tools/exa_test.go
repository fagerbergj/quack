package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// exaSample mirrors the real web_search_exa text output: Key: value header lines,
// a Highlights body, records separated by a "---" line.
const exaSample = `Title: TFI Bus Services | Transport for Ireland
URL: https://www.transportforireland.ie/getting-around/by-bus/tfi-bus-services/
Published: 2019-08-01T09:51:18.000Z
Author: N/A
Highlights:
Dublin Bus operates 130+ routes across Dublin.
Fleet of 1,000+ low-floor, wheelchair-accessible buses

---

Title: BusConnects Dublin Network Redesign - Transport for Ireland
URL: https://www.transportforireland.ie/network-redesign/
Published: 2022-06-02T14:10:20.000Z
Author: N/A
Highlights:
The new Dublin City bus network is now rolling out on a phased basis.
`

func TestParseExaResults(t *testing.T) {
	got := parseExaResults(exaSample)
	if len(got) != 2 {
		t.Fatalf("parsed %d results, want 2", len(got))
	}
	if got[0].Title != "TFI Bus Services | Transport for Ireland" {
		t.Errorf("title[0] = %q", got[0].Title)
	}
	if got[0].URL != "https://www.transportforireland.ie/getting-around/by-bus/tfi-bus-services/" {
		t.Errorf("url[0] = %q", got[0].URL)
	}
	if !strings.Contains(got[0].Snippet, "Dublin Bus operates 130+") {
		t.Errorf("snippet[0] missing highlights: %q", got[0].Snippet)
	}
	// The header lines (Published/Author) must not leak into the snippet.
	if strings.Contains(got[0].Snippet, "Published:") || strings.Contains(got[0].Snippet, "Author:") {
		t.Errorf("snippet[0] leaked header lines: %q", got[0].Snippet)
	}
	if got[1].URL != "https://www.transportforireland.ie/network-redesign/" {
		t.Errorf("url[1] = %q", got[1].URL)
	}
}

// A blank/preamble block (no URL) is skipped, not emitted as an empty result.
func TestParseExaResults_SkipsNonResultBlocks(t *testing.T) {
	if got := parseExaResults("Some preamble line with no fields\n"); len(got) != 0 {
		t.Fatalf("expected 0 results from a fieldless block, got %d", len(got))
	}
}

// TestParseExaREST covers the keyed JSON path: highlights become the snippet, and
// a result with no highlights falls back to its text body.
func TestParseExaREST(t *testing.T) {
	const body = `{"results":[
		{"title":"TFI Bus","url":"https://transportforireland.ie/","highlights":["Dublin Bus operates 130+ routes.","Wheelchair accessible."]},
		{"title":"No Highlights","url":"https://example.com/x","text":"fallback body text"},
		{"title":"Dropped","url":""}
	]}`
	got, _, err := parseExaREST(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseExaREST: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (the url-less one is dropped)", len(got))
	}
	if got[0].URL != "https://transportforireland.ie/" || !strings.Contains(got[0].Snippet, "Dublin Bus operates 130+") {
		t.Errorf("result[0] = %+v", got[0])
	}
	if got[1].Snippet != "fallback body text" {
		t.Errorf("result[1] snippet should fall back to text, got %q", got[1].Snippet)
	}
}

// TestExaSearchREST drives the keyed path end to end against a stub Exa: it must
// POST with the x-api-key header and the query in the body, then parse the JSON.
func TestExaSearchREST(t *testing.T) {
	var gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotQuery = body.Query
		_, _ = io.WriteString(w, `{"results":[{"title":"T","url":"https://e.com","highlights":["hi there"]}]}`)
	}))
	defer srv.Close()

	e := newExaSearcher("secret-key", srv.Client())
	e.restEndpoint = srv.URL

	res, _, err := e.Search(context.Background(), "dublin bus")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotKey != "secret-key" {
		t.Errorf("x-api-key header = %q, want secret-key", gotKey)
	}
	if gotQuery != "dublin bus" {
		t.Errorf("posted query = %q, want dublin bus", gotQuery)
	}
	if len(res) != 1 || res[0].URL != "https://e.com" || res[0].Snippet != "hi there" {
		t.Errorf("parsed result = %+v", res)
	}
}

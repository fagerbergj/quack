package tools

import (
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

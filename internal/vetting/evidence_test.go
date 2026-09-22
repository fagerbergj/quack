package vetting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"github.com/fagerbergj/quack/internal/recordstore"
)

type fakePages map[string][]byte

func (f fakePages) Latest(_ context.Context, id string) ([]byte, int, bool, error) {
	b, ok := f[id]
	return b, 1, ok, nil
}

// The web_page kind is registered by internal/tools, which this package cannot import
// (tools imports vetting); mirror its identity here: sha256 of the URL, first 12 hex.
var registerWebPage sync.Once

func pageID(t *testing.T, u string) string {
	registerWebPage.Do(func() {
		recordstore.Register(webPageKind, recordstore.KindSpec{Class: recordstore.Blob, RequiresHint: true, System: true,
			Identity: func(_ []byte, hint string) (string, error) {
				h := sha256.Sum256([]byte(hint))
				return hex.EncodeToString(h[:])[:12], nil
			}})
	})
	id, err := recordstore.IdentityFor(webPageKind, "", u)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLocateSpecific(t *testing.T) {
	page := "Revenue reached $1,234,500 in 2024, up 12.5% on the year; the 2012 figure was lower."
	cases := []struct {
		s    Specific
		want bool
	}{
		{Specific{Kind: "currency", Norm: "1234500"}, true},
		{Specific{Kind: "percent", Norm: "12.5"}, true},
		{Specific{Kind: "number", Norm: "12"}, false}, // inside 2012 does not count
		{Specific{Kind: "number", Norm: "2024"}, true},
		{Specific{Kind: "name", Norm: "revenue reached"}, true},
		{Specific{Kind: "number", Norm: "99"}, false},
	}
	for _, c := range cases {
		if _, got := LocateSpecific(page, c.s); got != c.want {
			t.Errorf("LocateSpecific(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestCheckUnits_LocatesThroughStoredPage(t *testing.T) {
	store := fakePages{pageID(t, "https://x.example/report"): []byte("Users rose 30% last year to 4,000.")}
	units := FindUnits("Users rose 30% to 4000 ([r](https://x.example/report/#top)).\n\nA claim with 77% and no source.")
	checks := CheckUnits(context.Background(), units, WebPageEvidence{Store: store})
	states := map[string]int{}
	for _, c := range checks {
		states[c.State]++
	}
	if states["located"] != 2 || states["uncited"] != 1 {
		t.Fatalf("states = %v, want 30%% and 4000 located through the fragment/slash-normalised URL and 77%% uncited: %+v", states, checks)
	}
	checks = CheckUnits(context.Background(), units[:1], WebPageEvidence{Store: fakePages{}})
	if checks[0].State != "no_stored_text" {
		t.Fatalf("state = %q, want no_stored_text when the page was never fetched", checks[0].State)
	}
}

func TestLocateSpecific_DateFormsAndMarkup(t *testing.T) {
	page := "Published September 15, 2026. Chart **tailored to a 12-team** league."
	if _, ok := LocateSpecific(page, Specific{Kind: "date", Norm: canonicalDate("Sep 15, 2026")}); !ok {
		t.Error("an ISO-normalised date should locate its long rendering")
	}
	if _, ok := LocateSpecific(page, Specific{Kind: "date", Norm: "2026-09-15"}); !ok {
		t.Error("2026-09-15 should locate 'September 15, 2026'")
	}
	if _, ok := LocateSpecific(page, Specific{Kind: "quote", Norm: normalizeSpecific("quote", "tailored to a **12-team** league")}); !ok {
		t.Error("markdown emphasis must not break a quote match")
	}
	if got := canonicalDate("15 Sep 2026"); got != "2026-09-15" {
		t.Errorf("canonicalDate = %q", got)
	}
}

func TestLocateQuote_SegmentsAndPunctuation(t *testing.T) {
	page := "The Wolf\u2019s Rest Of Season rankings \u2014 built weekly \u2014 drive the chart; values are **not** additive.\n"
	cases := map[string]bool{
		"the wolf's rest of season rankings":            true,
		"the wolf's rest of season ... drive the chart": true,
		"drive the chart ... rest of season":            false, // out of order
		"values are not additive":                       true,
		"the wolf's rest of season rankings are bogus":  false,
	}
	for q, want := range cases {
		if _, got := LocateSpecific(page, Specific{Kind: "quote", Norm: q}); got != want {
			t.Errorf("locate quote %q = %v, want %v", q, got, want)
		}
	}
}

func TestLocateSpecific_SignedFigureAndSecondCitation(t *testing.T) {
	for _, page := range []string{"the deficit was -12 last year", "totals:\n-12 on the year", "(-12)"} {
		if _, ok := LocateSpecific(page, Specific{Kind: "number", Norm: "12"}); ok {
			t.Errorf("12 must not locate inside -12 in %q", page)
		}
	}
	store := fakePages{pageID(t, "https://a.example/x"): []byte("nothing here"), pageID(t, "https://b.example/y"): []byte("growth of 30% in 2024")}
	units := FindUnits("Growth was 30% ([a](https://a.example/x), [b](https://b.example/y)).")
	checks := CheckUnits(context.Background(), units, WebPageEvidence{Store: store})
	if len(checks) != 1 || checks[0].State != "located" || checks[0].Citation != "https://b.example/y" {
		t.Fatalf("checks = %+v, want the figure located through the second citation", checks)
	}
}

func TestLocateSpecific_NumberWordsAndSecondLookWindow(t *testing.T) {
	if _, ok := LocateSpecific("only three of nine players saw more touches", Specific{Kind: "number", Norm: "9"}); !ok {
		t.Error("a spelled-out nine should locate the figure 9")
	}
	store := fakePages{pageID(t, "https://p.example/x"): []byte("The roster-need band is stated as ten to fifteen percent of parity by most charts.")}
	units := FindUnits("Charts allow a 15-25% roster-need band ([c](https://p.example/x)).")
	checks := CheckUnits(context.Background(), units, WebPageEvidence{Store: store})
	var got *UnitCheck
	for i := range checks {
		if checks[i].Specific.Value == "25%" {
			got = &checks[i]
		}
	}
	if got == nil || got.State != "unlocated" || !strings.Contains(got.Window, "parity") {
		t.Fatalf("25%% should be unlocated with a key-term window for the second look: %+v", got)
	}
}

func TestSecondLook_TermsIgnoreLinksAndCitationFollowsTheWindow(t *testing.T) {
	store := fakePages{
		pageID(t, "https://example.test/a"): []byte("an example passage about examples and nothing else"),
		pageID(t, "https://b.test/b"):       []byte("the roster band is stated as ten percent by this chart"),
	}
	units := FindUnits("The roster band is 25% ([a](https://example.test/a), [b](https://b.test/b)).")
	var got *UnitCheck
	for _, c := range CheckUnits(context.Background(), units, WebPageEvidence{Store: store}) {
		if c.Specific.Value == "25%" {
			c := c
			got = &c
		}
	}
	if got == nil || got.State != "unlocated" || got.Citation != "https://b.test/b" || !strings.Contains(got.Window, "roster band") {
		t.Fatalf("second look should use the claim's own words (not the URL's) and report the page its window came from: %+v", got)
	}
}

package vetting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

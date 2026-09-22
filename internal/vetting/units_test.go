package vetting

import (
	"strings"
	"testing"
)

func TestFindUnits_SentencesListRowsAndSpecifics(t *testing.T) {
	text := "Revenue grew 12.5% to $4.2M in Q2 2025 (2025-07-30). Alice Johnson led the team.\n\n" +
		"- 3 regions reported [1]\n- \"fastest quarter\" on record\n\n" +
		"| region | share |\n|---|---|\n| EU | 40% |\n\n" +
		"```\nignored 999 code\n```\n\n" +
		"[1]: https://example.com/report\n"
	units := FindUnits(text)
	kinds := map[string]int{}
	for _, u := range units {
		kinds[u.Kind]++
	}
	if kinds["sentence"] != 2 || kinds["list_item"] != 2 || kinds["table_row"] != 2 {
		t.Fatalf("kinds = %v, want 2 sentences, 2 list items, 2 table rows (header + EU): %+v", kinds, units)
	}
	first := units[0]
	want := map[string]string{"percent": "12.5", "currency": "4.2", "date": "2025-07-30", "number": "2025"}
	got := map[string]string{}
	for _, s := range first.Specifics {
		if _, ok := want[s.Kind]; ok && got[s.Kind] == "" {
			got[s.Kind] = s.Norm
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("first unit %s = %q, want %q (specifics %+v)", k, got[k], v, first.Specifics)
		}
	}
	if len(units[1].Specifics) != 0 {
		t.Errorf("a sentence with only a proper name has no specifics, got %+v", units[1].Specifics)
	}
	if !hasSpecific(units[3], "quote", "fastest quarter") {
		t.Errorf("quote not found in %+v", units[3].Specifics)
	}
	for _, u := range units {
		if strings.Contains(u.Text, "999") {
			t.Errorf("fenced code leaked into a unit: %q", u.Text)
		}
	}
}

func TestFindUnits_CitationPairing(t *testing.T) {
	text := "Users rose 30% last year. That is the fastest growth on record ([source](https://a.example/x)).\n\n" +
		"Unrelated paragraph with no link at all.\n\n" +
		"Marker cited claim of 55% [2].\n\n[2]: https://b.example/y"
	units := FindUnits(text)
	if len(units) != 4 {
		t.Fatalf("units = %d, want 4 (the reference list pairs with nothing): %+v", len(units), units)
	}
	if got := units[0].Citations; len(got) != 1 || got[0] != "https://a.example/x" {
		t.Errorf("uncited figure should pair with the block's next citation, got %v", got)
	}
	if len(units[2].Citations) != 0 {
		t.Errorf("a block with no citation must not borrow one from another block: %v", units[2].Citations)
	}
	if got := units[3].Citations; len(got) != 1 || got[0] != "https://b.example/y" {
		t.Errorf("[2] should resolve through the reference list, got %v", got)
	}
}

func hasSpecific(u Unit, kind, norm string) bool {
	for _, s := range u.Specifics {
		if s.Kind == kind && s.Norm == norm {
			return true
		}
	}
	return false
}

func TestFindUnits_IgnoresHeadingsLinkTargetsAndQualifiers(t *testing.T) {
	text := "# Report for 2025-01-01\n\nA 10-team league ([chart](https://x.example/2026-09-15/wk2)) scored 45 points in 2024."
	units := FindUnits(text)
	if len(units) != 1 {
		t.Fatalf("units = %d, want 1 (the heading is not a unit): %+v", len(units), units)
	}
	var norms []string
	for _, s := range units[0].Specifics {
		norms = append(norms, s.Kind+":"+s.Norm)
	}
	want := "number:45 number:2024"
	if strings.Join(norms, " ") != want {
		t.Fatalf("specifics = %v, want %q (no date from the URL, no 10 from 10-team)", norms, want)
	}
}

func TestFindUnits_SectionNumbersAreNotFigures(t *testing.T) {
	units := FindUnits("**2.3 Consolidate or hold** the star in a 12-team league, up 40%.")
	if len(units) != 1 || len(units[0].Specifics) != 1 || units[0].Specifics[0].Norm != "40" {
		t.Fatalf("specifics = %+v, want only the 40%% (2.3 numbers the section, 12-team qualifies)", units)
	}
}

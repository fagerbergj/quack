package vetting

import (
	"strings"
	"testing"
)

// TestRenderReviewOverview_Golden pins the fixed review format's rendered
// output for each verdict/scope shape the design calls out - one fixed
// format, generated in code, never free text (see reviewoverview.go's doc comment).
func TestRenderReviewOverview_Golden(t *testing.T) {
	cases := []struct {
		name string
		in   reviewOverviewInput
		want string
	}{
		{
			name: "approve_first_review",
			in: reviewOverviewInput{
				Verdict: "approve", ScopeKnown: true, FirstReview: true, FileCount: 5, HeadSHA: "abc1234567890",
				Takeaway: "Auth flow change is safe and well tested.",
				Verified: []string{"Ran the auth test suite locally", "Checked the token refresh path"},
				Notes:    []string{"Consider adding a changelog entry"},
			},
			want: "**Verdict: approve** · head abc1234\n\n" +
				"Scope: first review, whole PR (5 files)\n\n" +
				"Auth flow change is safe and well tested.\n\n" +
				"### Verified\n\n- Ran the auth test suite locally\n- Checked the token refresh path\n\n" +
				"### Notes\n\n- Consider adding a changelog entry",
		},
		{
			name: "request_changes_with_blockers",
			in: reviewOverviewInput{
				Verdict: "request_changes", ScopeKnown: true, FirstReview: true, FileCount: 3, HeadSHA: "def4567890123",
				Takeaway: "Two blocking issues, both in the fallback path.",
				Comments: []ReviewComment{
					{Path: "internal/server/router.go", Line: 42, Body: "🚨 **blocking:** route after SPA fallback never matches. This breaks all API calls."},
					{Path: "internal/db/pool.go", Line: 10, Body: "**blocking:** connection leak on error. Pool exhausts under load."},
					{Path: "internal/foo.go", Line: 5, Body: "suggestion: extract this into a helper. Improves readability."},
					{Path: "internal/bar.go", Line: 6, Body: "nit: rename var. Minor clarity."},
				},
			},
			// Every blocking finding is a row; suggestions/nits are counted
			// in the verdict line but never listed here (findings stay
			// inline - the Highlights table only surfaces blockers, or suggestions when there is no blocker at all).
			want: "**Verdict: request changes** · 2 blocking · 1 suggestion · 1 nit · head def4567\n\n" +
				"Scope: first review, whole PR (3 files)\n\n" +
				"Two blocking issues, both in the fallback path.\n\n" +
				"### Highlights\n\n| Severity | Where | Why it matters |\n| --- | --- | --- |\n" +
				"| blocking | internal/server/router.go:42 | route after SPA fallback never matches |\n" +
				"| blocking | internal/db/pool.go:10 | connection leak on error |",
		},
		{
			name: "comment_with_suggestions_only",
			in: reviewOverviewInput{
				Verdict: "comment", ScopeKnown: true, FirstReview: true, FileCount: 2, HeadSHA: "aaa1111111111",
				Takeaway: "Mostly style feedback, nothing blocking.",
				// No blocking finding: the table falls back to the top 2
				// suggestions in staged order - the third is dropped.
				Comments: []ReviewComment{
					{Path: "a.go", Line: 1, Body: "suggestion: use context.Context here. Clearer cancellation."},
					{Path: "b.go", Line: 2, Body: "suggestion: rename to camelCase. Matches convention."},
					{Path: "c.go", Line: 3, Body: "suggestion: third one should not appear. Too many."},
				},
			},
			want: "**Verdict: comment** · 3 suggestions · head aaa1111\n\n" +
				"Scope: first review, whole PR (2 files)\n\n" +
				"Mostly style feedback, nothing blocking.\n\n" +
				"### Highlights\n\n| Severity | Where | Why it matters |\n| --- | --- | --- |\n" +
				"| suggestion | a.go:1 | use context.Context here |\n" +
				"| suggestion | b.go:2 | rename to camelCase |",
		},
		{
			name: "rereview_with_resolved_and_dismissed",
			in: reviewOverviewInput{
				Verdict: "request_changes", ScopeKnown: true, FirstReview: false, PriorHeadSHA: "bbb2222222222", CommitsSinceKnown: true, CommitsSince: 3, FileCount: 4, HeadSHA: "ccc3333333333",
				Takeaway:   "One new blocker since last review.",
				SinceKnown: true, Resolved: 2, Open: 1,
				Dismissed: []DismissedEntry{{Path: "x.go", Line: 9, Note: "not a real issue"}},
				Comments:  []ReviewComment{{Path: "y.go", Line: 20, Body: "blocking: still broken here. Needs a fix."}},
			},
			want: "**Verdict: request changes** · 1 blocking · head ccc3333\n\n" +
				"Scope: re-review, 3 commits since bbb2222 (4 files)\n\n" +
				"One new blocker since last review.\n\n" +
				"Since last review: 2 resolved · 1 open · 1 dismissed (x.go:9: not a real issue)\n\n" +
				"### Highlights\n\n| Severity | Where | Why it matters |\n| --- | --- | --- |\n" +
				"| blocking | y.go:20 | still broken here |",
		},
		{
			// nit 9: a prior head is known but rev-list couldn't resolve a
			// commit count (force-pushed away) - the whole "N commits
			// since <sha7>" clause is omitted, never "0 commits since".
			name: "rereview_unresolved_commit_count",
			in: reviewOverviewInput{
				Verdict: "approve", ScopeKnown: true, FirstReview: false, PriorHeadSHA: "bbb2222222222", CommitsSinceKnown: false, FileCount: 4, HeadSHA: "ccc3333333333",
				Takeaway: "Looks fine now.",
			},
			want: "**Verdict: approve** · head ccc3333\n\n" +
				"Scope: re-review (4 files)\n\n" +
				"Looks fine now.",
		},
		{
			// nit 10: a label with no explanation at all falls back to the
			// finding's own path for Why, rather than an empty cell.
			name: "label_only_body_falls_back_to_path",
			in: reviewOverviewInput{
				Verdict: "request_changes",
				Comments: []ReviewComment{
					{Path: "internal/gate.go", Line: 7, Body: "blocking:"},
				},
			},
			want: "**Verdict: request changes** · 1 blocking\n\n" +
				"### Highlights\n\n| Severity | Where | Why it matters |\n| --- | --- | --- |\n" +
				"| blocking | internal/gate.go:7 | internal/gate.go |",
		},
		{
			// suggestion 4: a literal "|" in a finding's title/why must not
			// break the Highlights table it renders into.
			name: "pipe_in_why_is_escaped",
			in: reviewOverviewInput{
				Verdict: "request_changes",
				Comments: []ReviewComment{
					{Path: "internal/parse.go", Line: 3, Body: "blocking: the a|b union type is never checked. Crashes on the second variant."},
				},
			},
			want: "**Verdict: request changes** · 1 blocking\n\n" +
				"### Highlights\n\n| Severity | Where | Why it matters |\n| --- | --- | --- |\n" +
				"| blocking | internal/parse.go:3 | the a\\|b union type is never checked |",
		},
		{
			name: "legacy_summary_record",
			in: reviewOverviewInput{
				Verdict:       "approve",
				LegacySummary: "This is a long free-text summary from before the migration to structured fields, kept for history.",
			},
			// A pre-migration record has no Takeaway/Verified/Notes: only
			// the verdict line renders plus the legacy summary, folded
			// into Notes rather than lost.
			want: "**Verdict: approve**\n\n" +
				"### Notes\n\n- This is a long free-text summary from before the migration to structured fields, kept for history.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderReviewOverview(c.in)
			if got != c.want {
				t.Fatalf("render mismatch:\n got:  %q\n want: %q", got, c.want)
			}
		})
	}
}

// TestRenderReviewOverview_LegacySummaryTruncated proves a long pre-migration
// summary is capped, not reproduced at 1,000+ characters (the exact defect
// this format replaces).
func TestRenderReviewOverview_LegacySummaryTruncated(t *testing.T) {
	long := strings.Repeat("x", legacySummaryDisplayCap+200)
	got := renderReviewOverview(reviewOverviewInput{Verdict: "comment", LegacySummary: long})
	if r := []rune(got); len(r) > legacySummaryDisplayCap+60 {
		t.Fatalf("legacy summary not truncated: %d runes", len(r))
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("truncated summary missing the truncation marker: %q", got)
	}
}

// TestCommentLabel_Variants pins the Conventional-Comments label parsing
// against every variant the design names, plus a decorated label.
func TestCommentLabel_Variants(t *testing.T) {
	cases := []struct {
		body      string
		wantLabel string
		wantWhy   string
	}{
		{"🚨 **blocking:** route after SPA fallback never matches. More detail.", "blocking", "route after SPA fallback never matches"},
		{"**blocking:** connection leak on error. Pool exhausts under load.", "blocking", "connection leak on error"},
		{"blocking: nil deref on the empty path. Crashes on startup.", "blocking", "nil deref on the empty path"},
		{"blocking (security): SQL injection via unescaped input. Sanitize it.", "blocking", "SQL injection via unescaped input"},
		{"suggestion: extract this into a helper. Improves readability.", "suggestion", "extract this into a helper"},
		{"nit: rename var. Minor clarity.", "nit", "rename var"},
		{"question: why is this synchronous? Seems avoidable.", "question", "why is this synchronous? Seems avoidable"},
		{"no label at all here, just prose.", "", "no label at all here, just prose"},
		// #1: a period only ends a sentence when followed by whitespace -
		// none of these mid-token periods are, so the sentence runs on.
		{"blocking: cfg.Setup is read before validation runs. Fix the order.", "blocking", "cfg.Setup is read before validation runs"},
		{"blocking: see internal/router.go:42 for the bad route. It never matches.", "blocking", "see internal/router.go:42 for the bad route"},
		{"blocking: the docs link https://example.com/a.b.c is dead. Update it.", "blocking", "the docs link https://example.com/a.b.c is dead"},
		// Abbreviations don't end the sentence at their own period either.
		{"suggestion: prefer a typed error, e.g. sentinel errors. Cleaner call sites.", "suggestion", "prefer a typed error, e.g. sentinel errors"},
		{"suggestion: use a stable id, i.e. the hash, not the index. Survives reorders.", "suggestion", "use a stable id, i.e. the hash, not the index"},
		{"nit: prefer a slice vs. a map here. Simpler iteration.", "nit", "prefer a slice vs. a map here"},
		// A period inside a backtick span never ends the sentence.
		{"blocking: `a.b.c()` returns nil on error. Callers don't check it.", "blocking", "`a.b.c()` returns nil on error"},
		// A label with nothing after it - the render loop falls back to the
		// finding's path, but commentLabel itself reports an empty why.
		{"blocking:", "blocking", ""},
	}
	for _, c := range cases {
		label, why := commentLabel(c.body)
		if label != c.wantLabel || why != c.wantWhy {
			t.Errorf("commentLabel(%q) = (%q, %q), want (%q, %q)", c.body, label, why, c.wantLabel, c.wantWhy)
		}
	}
}

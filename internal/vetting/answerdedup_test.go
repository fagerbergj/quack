package vetting

import "testing"

// reviewBody: 17 unique words, well over minDedupBodyLen, used to derive
// verbatim/paraphrase/dilute variants below without hand-counting overlap
// on every table row.
const reviewBody = "blocking the retry loop never releases mutex on error path and it causes a permanent deadlock now"

func TestRestatesRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		answer, body string
		want         bool
	}{
		{
			name:   "verbatim restatement",
			answer: "Here's my review:\n\n" + reviewBody,
			body:   reviewBody,
			want:   true,
		},
		{
			name: "paraphrase over threshold",
			// same 17 words minus 2 (blocking, now), reordered - not a
			// substring match, but 15/17 = 0.88 word overlap.
			answer: "the retry loop never releases mutex on error path and it causes a permanent deadlock issue",
			body:   reviewBody,
			want:   true,
		},
		{
			name: "paraphrase under threshold kept",
			// only 5 of 17 body words survive (the, mutex, on, path, deadlock).
			answer: "the mutex handling on this path looks fine to me, no deadlock risk here",
			body:   reviewBody,
			want:   false,
		},
		{
			name:   "short body ignored",
			answer: "LGTM",
			body:   "LGTM",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := restatesRecord(tt.answer, tt.body); got != tt.want {
				t.Errorf("restatesRecord(%q, %q) = %v, want %v", tt.answer, tt.body, got, tt.want)
			}
		})
	}
}

func TestDedupeAnswerAgainstStaged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer string
		staged map[string]StagedDelivery
		want   string // "" means "answer unchanged"
	}{
		{
			name:   "recovered record skipped even when verbatim",
			answer: reviewBody,
			staged: map[string]StagedDelivery{
				"review": {Kind: "review", Body: reviewBody, Recovered: true},
			},
		},
		{
			name:   "multiple staged records, only one matches",
			answer: "Here's my review:\n\n" + reviewBody,
			staged: map[string]StagedDelivery{
				"pr":     {Kind: "pull_request", Title: "Add retry backoff", Body: "adds an exponential backoff helper and wires it into the retry loop's error path"},
				"review": {Kind: "review", Event: "request_changes", Body: reviewBody, Comments: []ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}},
			},
			want: "Staged: request_changes (1 inline comment).",
		},
		{
			name: "answer adds new content beyond the record kept",
			// echoes a sliver of the staged PR body, then goes on to raise
			// unrelated points the record never mentions.
			answer: "Wired the retry loop through. Also flagged a flaky test in CI and suggested we add a metrics counter for retry exhaustion.",
			staged: map[string]StagedDelivery{
				"pr": {Kind: "pull_request", Title: "Add retry backoff", Body: "adds an exponential backoff helper and wires it into the retry loop's error path"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := tt.want
			if want == "" {
				want = tt.answer
			}
			if got := dedupeAnswerAgainstStaged(tt.answer, tt.staged); got != want {
				t.Errorf("dedupeAnswerAgainstStaged() = %q, want %q", got, want)
			}
		})
	}
}

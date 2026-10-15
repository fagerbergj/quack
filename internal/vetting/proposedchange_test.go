package vetting

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/recordstore"
)

func TestProposedChangeCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  *ProposedChange
		wantErr string
	}{
		{"nil", nil, ""},
		{"valid", &ProposedChange{Language: "go", Code: "return err"}, ""},
		{"empty code", &ProposedChange{Language: "go", Code: " \n"}, "proposed_change.code is empty"},
		{"suggestion", &ProposedChange{Language: "suggestion", Code: "x"}, `can't be "suggestion"`},
		{"not a token", &ProposedChange{Language: "go lang", Code: "x"}, "short language tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.change.Check()
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Check() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestProposedChangeFenceOutgrowsInnerFence(t *testing.T) {
	got := (&ProposedChange{Language: "md", Code: "```go\nx\n```\n"}).Fence()
	if want := "\n\n````md\n```go\nx\n```\n````"; got != want {
		t.Fatalf("Fence() = %q, want %q", got, want)
	}
}

func TestValidateFindingRejectsEmptyProposedCode(t *testing.T) {
	raw, _ := json.Marshal(FindingRecord{Title: "t", ProposedChange: &ProposedChange{Language: "go"}})
	err := validateFinding(raw)
	for _, want := range []string{"path is required", "proposed_change.code is empty"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("validateFinding = %v, want every problem incl. %q", err, want)
		}
	}
}

// A finding's proposed change reaches the inline comment as a plain go fence, not the Highlights table.
func TestRenderReviewFromArtifact_AppendsProposedChange(t *testing.T) {
	cfg := Config{IsReviewer: true, ChatID: "ext:github:owner-repo-47", User: "u1", Artifacts: artifact.InMemoryService()}
	finding := FindingRecord{Path: "a.go", LineHint: 10, Title: "blocking: unchecked error", Rationale: "err is dropped", State: "new",
		ProposedChange: &ProposedChange{Language: "go", Code: "if err != nil {\n\treturn err\n}"}}
	fid, err := recordstore.IdentityFor(kindFinding, finding, "")
	if err != nil {
		t.Fatalf("finding identity: %v", err)
	}
	seedCodeReview(t, cfg, "request_changes", "", map[string]FindingRecord{fid: finding})

	item, ok := renderReviewFromArtifact(context.Background(), cfg, "n1")
	if !ok {
		t.Fatal("renderReviewFromArtifact: no record")
	}
	want := "blocking: unchecked error: err is dropped\n\n```go\nif err != nil {\n\treturn err\n}\n```"
	if len(item.Comments) != 1 || item.Comments[0].Body != want {
		t.Fatalf("Comments = %+v, want body %q", item.Comments, want)
	}
	if strings.Contains(item.Body, "```") {
		t.Fatalf("Body = %q, want the code kept out of the overview table", item.Body)
	}
}

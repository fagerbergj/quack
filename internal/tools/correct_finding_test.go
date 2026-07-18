package tools

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/memory"
)

func TestFalsePositiveCandidate_RequiresEveryField(t *testing.T) {
	base := correctReviewFindingArgs{Owner: "acme", Repo: "games", PRNumber: 246, Finding: "f", Reason: "r"}
	cases := map[string]correctReviewFindingArgs{
		"missing owner":     {Repo: base.Repo, PRNumber: base.PRNumber, Finding: base.Finding, Reason: base.Reason},
		"missing repo":      {Owner: base.Owner, PRNumber: base.PRNumber, Finding: base.Finding, Reason: base.Reason},
		"missing pr_number": {Owner: base.Owner, Repo: base.Repo, Finding: base.Finding, Reason: base.Reason},
		"missing finding":   {Owner: base.Owner, Repo: base.Repo, PRNumber: base.PRNumber, Reason: base.Reason},
		"missing reason":    {Owner: base.Owner, Repo: base.Repo, PRNumber: base.PRNumber, Finding: base.Finding},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := falsePositiveCandidate(a); err == nil {
				t.Fatalf("falsePositiveCandidate(%+v) should error", a)
			}
		})
	}
}

func TestFalsePositiveCandidate_ScopesToRepoBucketAttributed(t *testing.T) {
	sc, cand, err := falsePositiveCandidate(correctReviewFindingArgs{
		Owner:    "Acme",
		Repo:     "Games",
		PRNumber: 246,
		Finding:  `empty Comment.Body breaks dispatch via triggerTask`,
		Reason:   "dispatch takes the task string directly, it never calls triggerTask",
	})
	if err != nil {
		t.Fatalf("falsePositiveCandidate: %v", err)
	}
	// Same key format as workspace.RepoIdentity ("github.com/owner/repo",
	// lowercased) — the exact bucket the gate's memoryScope resolves for a
	// coding node working in this repo.
	if sc.Repo != "github.com/acme/games" {
		t.Fatalf("scope repo = %q, want github.com/acme/games", sc.Repo)
	}
	if sc.Role != memory.RoleCoding {
		t.Fatalf("scope role = %q, want %q", sc.Role, memory.RoleCoding)
	}
	for _, want := range []string{"PR #246", "triggerTask", "dispatch takes the task string directly"} {
		if !strings.Contains(cand.Content, want) {
			t.Fatalf("candidate content missing %q: %q", want, cand.Content)
		}
	}
	if cand.Metadata["kind"] != "false_positive" {
		t.Fatalf("candidate kind = %q, want false_positive", cand.Metadata["kind"])
	}
}

func TestNewCorrectReviewFindingTool(t *testing.T) {
	tl, err := NewCorrectReviewFindingTool(nil) // construction only; handler not invoked
	if err != nil {
		t.Fatalf("NewCorrectReviewFindingTool: %v", err)
	}
	if tl.Name() != "correct_review_finding" {
		t.Fatalf("name = %q, want correct_review_finding", tl.Name())
	}
}

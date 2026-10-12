package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

// pullCommentBody is issueCommentBody-shaped but the issue is a pull request
// (GitHub marks PR comments with a non-null issue.pull_request).
func pullCommentBody(commentBody string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7,"pull_request":{}},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody))
}

// seedGC builds a first-load githubContext: no prior snapshot, so delta stays nil and the envelope seeds everything.
func seedGC(snap Snapshot, excludeCommentID int64) githubContext {
	_ = excludeCommentID // commentsBlock reads exclusion from issueCommentPayload.Comment.ID
	return githubContext{snap: snap, firstLoad: true}
}

// fakeIntentClassifier returns fixed verdicts; errAlways fails outright. Its three prompts are told apart by content,
// and the granted*/issue* fields degrade one classifier independently of the others.
type fakeIntentClassifier struct {
	verdict               string // "WORK" or "CONVERSATIONAL", or any other/blank to test the unparseable path
	grantedDeliverable    string // "REPLY", "REVIEW", or "COMMIT", or any other/blank to test the unparseable path
	issueDeliverable      string // "IMPLEMENT" or "COMMENT", or any other/blank to test the unparseable path
	errAlways             error
	grantedDeliverableErr error
	issueDeliverableErr   error
	calls                 int32
}

func (f *fakeIntentClassifier) Classify(_ context.Context, prompt string) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.errAlways != nil {
		return "", f.errAlways
	}
	if strings.Contains(prompt, "REPLY, REVIEW, or COMMIT") {
		if f.grantedDeliverableErr != nil {
			return "", f.grantedDeliverableErr
		}
		return f.grantedDeliverable, nil
	}
	if strings.Contains(prompt, "IMPLEMENT or COMMENT") {
		if f.issueDeliverableErr != nil {
			return "", f.issueDeliverableErr
		}
		return f.issueDeliverable, nil
	}
	return f.verdict, nil
}

// A 12,000-char issue body (under seedCap) reaches the envelope whole, no ellipsis.
func TestBuildEnvelopeLargeIssueBodyReachesIntact(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	body := strings.Repeat("a", 12000)
	var issue issueCommentPayload
	issue.Issue.Number = 42
	issue.Repository.Name, issue.Repository.Owner.Login = "widgets", "acme"

	env := ext.buildEnvelope(context.Background(), issue, "add a feature", seedGC(Snapshot{Body: body}, 0), nil, nil)

	if !strings.Contains(env, body) {
		t.Fatalf("12000-char issue body did not reach the envelope intact:\n%s", truncateForLog(env))
	}
	if strings.Contains(env, "…") || strings.Contains(env, "TRUNCATED") {
		t.Errorf("envelope should carry no truncation marker for a body under the seed cap:\n%s", truncateForLog(env))
	}
}

// A description over seedCap is marked truncated and points at the full file: the one sanctioned truncation.
func TestBuildEnvelopeTruncatesOversizedDescription(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	body := strings.Repeat("x", seedCap+5000)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{Body: body}, 0), nil, nil)

	if !strings.Contains(env, "TRUNCATED") {
		t.Errorf("an oversized description should be marked truncated:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, "issue.json") {
		t.Errorf("a truncated issue description should point at issue.json:\n%s", truncateForLog(env))
	}
	if strings.Contains(env, body) {
		t.Errorf("an oversized description should not appear in full")
	}
}

// TestBuildEnvelopeTruncatesOversizedPRDescription pins the PR half of the
// same ceiling - it must point at pull.json, not issue.json.
func TestBuildEnvelopeTruncatesOversizedPRDescription(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	body := strings.Repeat("x", seedCap+5000)
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	env := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true, Body: body}, 0), nil, nil)

	if !strings.Contains(env, "pull.json") {
		t.Errorf("a truncated PR description should point at pull.json:\n%s", truncateForLog(env))
	}
}

// A comment deleted between dispatches shows in the delta (diffSnapshots); GitHub's ?since= can't express deletions.
func TestBuildEnvelopeCommentDeletionVisibleInDelta(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	old := Snapshot{Comments: []snapshotComment{{ID: 1, User: "bob", Body: "will be deleted", CreatedAt: "t0"}}}
	cur := Snapshot{Comments: []snapshotComment{}}
	delta := diffSnapshots(old, cur, 0)
	gh := githubContext{snap: cur, delta: &delta}

	var issue issueCommentPayload
	issue.Issue.Number = 7

	env := ext.buildEnvelope(context.Background(), issue, "what changed?", gh, nil, nil)

	if !strings.Contains(env, `deleted="1"`) {
		t.Errorf("envelope missing the deleted=1 comment-delta marker:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, "will be deleted") {
		t.Errorf("a deleted comment's last-known body should still be visible in the delta:\n%s", truncateForLog(env))
	}
}

// An edited title re-seeds marked as changed, quoting the old value; an unchanged title is not marked.
func TestBuildEnvelopeTitleChangeMarking(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	old := Snapshot{Title: "Old title"}
	cur := Snapshot{Title: "New title"}
	changed := diffSnapshots(old, cur, 0)
	gh := githubContext{snap: cur, delta: &changed}

	env := ext.buildEnvelope(context.Background(), issue, "task", gh, nil, nil)
	if !strings.Contains(env, `New title (changed from "Old title")`) {
		t.Errorf("a changed title should be marked and quote the old value:\n%s", truncateForLog(env))
	}

	unchanged := diffSnapshots(cur, cur, 0)
	ghSame := githubContext{snap: cur, delta: &unchanged}
	envSame := ext.buildEnvelope(context.Background(), issue, "task", ghSame, nil, nil)
	if strings.Contains(envSame, "changed from") {
		t.Errorf("an unchanged title should not be marked changed:\n%s", truncateForLog(envSame))
	}
}

// <permissions> states exactly the closed vocabulary (pull_request, review, comment) that staged delivery
// and the trust gate's allowlist use.
func TestPermissionsTextRendersAllowedKinds(t *testing.T) {
	for _, tt := range []struct {
		name  string
		kinds []string
		want  string
	}{
		{"review-only", []string{"review", "comment"}, "review, comment"},
		{"issue-plan", []string{"comment"}, "comment"},
		{"fix", []string{"pull_request", "comment"}, "pull_request, comment"},
		{"none", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := permissionsText(tt.kinds); got != tt.want {
				t.Errorf("permissionsText(%+v) = %q, want %q", tt.kinds, got, tt.want)
			}
		})
	}
}

// A review-only run's envelope never grants pull_request: permissions state only what this run may do.
func TestBuildEnvelopeReviewOnlyPermissionsNeverNameStagePR(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	allowedKinds := []string{"review", "comment"} // PR-scoped: post_review + join_pr_conversation, no push
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("unused"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pr.isLabelTrigger = true

	env := ext.buildEnvelope(context.Background(), pr, autoReviewTask, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	permLine := env[strings.Index(env, "<permissions>") : strings.Index(env, "</permissions>")+len("</permissions>")]
	if strings.Contains(permLine, "pull_request") {
		t.Errorf("review-only envelope must not grant the pull_request kind:\n%s", permLine)
	}
	if !strings.Contains(permLine, "review") {
		t.Errorf("review-only envelope missing its own granted permission:\n%s", truncateForLog(env))
	}
}

// buildEnvelope renders one <artifact> line per input artifact, with its revision and new/unchanged status.
func TestBuildEnvelopeArtifactsManifestListsEntries(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	manifest := []artifactEntry{
		{Name: "comments", Revision: 4, Changed: true, Note: "47 items"},
		{Name: "event", Revision: 4, Changed: false, Note: "issues.labeled"},
	}
	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, manifest)

	if !strings.Contains(env, `<artifact id="bytes:comments" revision="4" status="new">47 items</artifact>`) {
		t.Errorf("envelope missing the comments manifest entry:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, `<artifact id="bytes:event" revision="4" status="unchanged">issues.labeled</artifact>`) {
		t.Errorf("envelope missing the event manifest entry:\n%s", truncateForLog(env))
	}
}

// No input artifacts means no <artifacts> block at all, not an empty or malformed one.
func TestBuildEnvelopeNoArtifactsBlockWithoutEntries(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 7
	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(env, "<artifacts") {
		t.Errorf("envelope should have no <artifacts> block when nothing was written:\n%s", truncateForLog(env))
	}
}

// A large PR's envelope carries churn summary and full file list (names only) while the description
// still arrives in full: the split applies to evidence, never the ask.
func TestBuildEnvelopeFortyFilePRHasChurnAndFullListNoContentButFullDescription(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	body := strings.Repeat("this PR description matters. ", 50)
	files := make([]changedFile, 40)
	for i := range files {
		files[i] = changedFile{Filename: fmt.Sprintf("internal/pkg%d/file.go", i), Additions: 10, Deletions: 5, Status: "modified"}
	}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	env := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true, Body: body, Files: files}, 0), nil, nil)

	if !strings.Contains(env, `<changed_files count="40" additions="400" deletions="200">`) {
		t.Errorf("envelope missing the churn summary for a 40-file PR:\n%s", truncateForLog(env))
	}
	for i := range files {
		if !strings.Contains(env, fmt.Sprintf("internal/pkg%d/file.go", i)) {
			t.Fatalf("envelope missing changed file %d of 40 - the orchestrator needs the FULL list to cluster by subsystem:\n%s", i, truncateForLog(env))
		}
	}
	if !strings.Contains(env, body) {
		t.Errorf("envelope missing the full PR description:\n%s", truncateForLog(env))
	}
}

// A reviewer sees CI status as compact lines (name: status[, conclusion]) plus a failing/pending/passing summary.
func TestChecksBlockRendersStatusAndSummary(t *testing.T) {
	checks := []checkRunView{
		{Name: "go-test", Status: "completed", Conclusion: "failure"},
		{Name: "docker-build", Status: "in_progress"},
		{Name: "lint", Status: "completed", Conclusion: "success"},
	}
	got := checksBlock(checks)
	for _, want := range []string{
		`<checks count="3" summary="1 failing, 1 pending, 1 passing">`,
		"go-test: completed failure",
		"docker-build: in_progress",
		"lint: completed success",
		"</checks>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("checksBlock missing %q:\n%s", want, got)
		}
	}
}

// A failing check's annotations or output title reach the envelope as indented why-lines.
func TestChecksBlockRendersWhyLinesForFailures(t *testing.T) {
	checks := []checkRunView{
		{Name: "go-test", Status: "completed", Conclusion: "failure",
			Why: []string{"internal/dag/executor.go:42 TestFoo: got 2, want 1", "build failed"}},
		{Name: "lint", Status: "completed", Conclusion: "success"},
	}
	got := checksBlock(checks)
	for _, want := range []string{
		"go-test: completed failure",
		"  why: internal/dag/executor.go:42 TestFoo: got 2, want 1",
		"  why: build failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("checksBlock missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "lint: completed success\n  why:") {
		t.Error("a passing check must not carry why-lines")
	}
}

// TestChecksBlockEmptyWhenNoChecks pins the degrade path: a PR with no check
// runs yet (or a fetch that failed upstream) gets no <checks> block at all.
func TestChecksBlockEmptyWhenNoChecks(t *testing.T) {
	if got := checksBlock(nil); got != "" {
		t.Errorf("checksBlock(nil) = %q, want empty", got)
	}
}

// A huge matrix build is capped and marked truncated, while the summary still counts every check.
func TestChecksBlockTruncatesAtCap(t *testing.T) {
	checks := make([]checkRunView, 35)
	for i := range checks {
		checks[i] = checkRunView{Name: fmt.Sprintf("job-%d", i), Status: "completed", Conclusion: "success"}
	}
	checks[30] = checkRunView{Name: "go-test", Status: "completed", Conclusion: "failure"}

	got := checksBlock(checks)
	if !strings.Contains(got, `count="35"`) {
		t.Errorf("checksBlock should still report the true total count:\n%s", got)
	}
	if !strings.Contains(got, "1 failing, 0 pending, 34 passing") {
		t.Errorf("checksBlock summary should count every check, not just the shown ones:\n%s", got)
	}
	if !strings.Contains(got, `truncated="showing 20 of 35"`) {
		t.Errorf("checksBlock missing a truncation marker for 35 checks:\n%s", got)
	}
	if strings.Contains(got, "job-25") {
		t.Errorf("checksBlock should not render a check past the cap:\n%s", got)
	}
	if !strings.Contains(got, "job-19") {
		t.Errorf("checksBlock should render every check up to the cap:\n%s", got)
	}
}

// A PR's check runs reach <checks> right after <changed_files>; a non-PR envelope never has the section.
func TestBuildEnvelopeIncludesChecksSection(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	checks := []checkRunView{{Name: "go-test", Status: "completed", Conclusion: "failure"}}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	gh := githubContext{snap: Snapshot{IsPR: true}, firstLoad: true, checks: checks}
	env := ext.buildEnvelope(context.Background(), pr, "review this", gh, nil, nil)
	if !strings.Contains(env, "go-test: completed failure") {
		t.Errorf("PR envelope missing the <checks> section:\n%s", truncateForLog(env))
	}
	if idx := strings.Index(env, "<changed_files"); idx == -1 || !strings.Contains(env[idx:], "<checks") {
		t.Errorf("<checks> should follow <changed_files>:\n%s", truncateForLog(env))
	}

	var issue issueCommentPayload
	issue.Issue.Number = 7
	issueEnv := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(issueEnv, "<checks") {
		t.Errorf("an issue (non-PR) envelope must never carry a <checks> section:\n%s", truncateForLog(issueEnv))
	}
}

// Unlike changed_files, CI status is compact enough that a review node gets it directly.
func TestBuildWorkerAskIncludesChecksSection(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	checks := []checkRunView{{Name: "go-test", Status: "completed", Conclusion: "failure"}}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowedKinds := []string{"review", "comment"}

	gh := githubContext{snap: Snapshot{IsPR: true}, firstLoad: true, checks: checks}
	ask := ext.buildWorkerAsk(context.Background(), pr, "review this", gh, allowedKinds, nil)
	if !strings.Contains(ask, "go-test: completed failure") {
		t.Errorf("worker ask missing the <checks> section a reviewer needs to avoid approving red CI:\n%s", truncateForLog(ask))
	}
}

// A node's background carries permissions, deliverable and the full ask, never changed_files evidence.
func TestBuildWorkerAskOmitsEvidenceKeepsAsk(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	body := "the worker still needs the full description"
	files := []changedFile{{Filename: "a.go", Additions: 1, Deletions: 1, Status: "modified"}}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowedKinds := []string{"review", "comment"}

	ask := ext.buildWorkerAsk(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true, Body: body, Files: files}, 0), allowedKinds, nil)

	if strings.Contains(ask, "changed_files") {
		t.Errorf("worker ask must not carry the orchestrator's changed_files evidence:\n%s", truncateForLog(ask))
	}
	if !strings.Contains(ask, body) {
		t.Errorf("worker ask missing the full description:\n%s", truncateForLog(ask))
	}
	if !strings.Contains(ask, "review") {
		t.Errorf("worker ask missing its permissions:\n%s", truncateForLog(ask))
	}
}

// truncateForLog keeps a failed test's diagnostic readable.
func truncateForLog(s string) string {
	const max = 4000
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated for log)"
}

// A deleted comment reads like a live one, so the delta must mark each item: miscount once and a
// retracted statement is treated as current.
func TestCommentsBlockMarksEachDeltaItem(t *testing.T) {
	gh := githubContext{delta: &Delta{
		CommentsAdded:   []snapshotComment{{ID: 1, User: "a", Body: "new one"}},
		CommentsEdited:  []snapshotComment{{ID: 2, User: "b", Body: "edited one"}},
		CommentsDeleted: []snapshotComment{{ID: 3, User: "c", Body: "retracted one"}},
	}}
	got := commentsBlock(gh, 0)
	for _, want := range []string{
		`"id":1,`, `"quack_status":"new"`,
		`"id":2,`, `"quack_status":"edited"`,
		`"id":3,`, `"quack_status":"deleted"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("commentsBlock missing %q:\n%s", want, got)
		}
	}
}

// Full-seed mode has no delta, so no item carries a status - the field is
// omitempty precisely so a seed stays GitHub's own shape.
func TestCommentsBlockSeedHasNoStatus(t *testing.T) {
	gh := githubContext{snap: Snapshot{Comments: []snapshotComment{{ID: 1, User: "a", Body: "hi"}}}}
	if got := commentsBlock(gh, 0); strings.Contains(got, "quack_status") {
		t.Errorf("full seed must not mark items:\n%s", got)
	}
}

// Constant instructions (which skill to load, how stage_pr/stage_review work) belong in the bundle prompt,
// never trigger prose; covers every deliverableText branch.
func TestNoTriggerOutputNamesSkillOrToolMechanics(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}

	var issue issueCommentPayload
	issue.Issue.Number = 7
	issue.Repository.Name, issue.Repository.Owner.Login = "widgets", "acme"

	var planOnly issueCommentPayload
	planOnly.Issue.Number = 7
	planOnly.planOnly = true
	planOnly.isLabelTrigger = true

	var implement issueCommentPayload
	implement.Issue.Number = 7
	implement.isLabelTrigger = true

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("please review this PR"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cases := map[string]string{
		"plan-only":      ext.buildEnvelope(context.Background(), planOnly, "plan it", seedGC(Snapshot{}, 0), nil, nil),
		"implement":      ext.buildEnvelope(context.Background(), implement, "implement it", seedGC(Snapshot{}, 0), nil, nil),
		"review-only":    ext.buildEnvelope(context.Background(), pr, "please review this PR", seedGC(Snapshot{IsPR: true, HeadRef: "x"}, 0), nil, nil),
		"conversational": ext.buildEnvelope(context.Background(), issue, "what changed?", seedGC(Snapshot{}, 0), nil, nil),
	}
	for name, env := range cases {
		for _, banned := range []string{"present-coding-plan", "stage_pr", "stage_review", "github_add_review_comment"} {
			if strings.Contains(env, banned) {
				t.Errorf("%s envelope names %q - tool/skill mechanics belong in the agent bundle prompt, not the trigger:\n%s", name, banned, truncateForLog(env))
			}
		}
	}
}

func TestTruncatedTextCutsOnRuneBoundary(t *testing.T) {
	s := strings.Repeat("a", seedCap-1) + "é" + "tail" // é's two bytes straddle seedCap
	got := truncatedText(s, "issue.json")
	if !utf8.ValidString(got) {
		t.Fatal("truncated text is not valid UTF-8")
	}
	body := got[strings.Index(got, "\n\n")+2:]
	if len(body) != seedCap-1 || !strings.Contains(got, fmt.Sprintf("showing the first %d", seedCap-1)) {
		t.Fatalf("kept %d bytes; note: %q", len(body), got[:strings.Index(got, "\n\n")])
	}
}

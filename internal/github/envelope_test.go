package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
)

// TestBuildEnvelopeLargeIssueBodyReachesIntact pins #666's test case 1: a
// 12,000-char issue body (well under seedCap) reaches the envelope whole, no
// ellipsis anywhere.
func TestBuildEnvelopeLargeIssueBodyReachesIntact(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	body := strings.Repeat("a", 12000)
	var issue issueCommentPayload
	issue.Issue.Number = 42
	issue.Repository.Name, issue.Repository.Owner.Login = "widgets", "acme"

	env := ext.buildEnvelope(context.Background(), issue, "add a feature", seedGC(Snapshot{Body: body}, 0), vetting.Grant{}, "", nil)

	if !strings.Contains(env, body) {
		t.Fatalf("12000-char issue body did not reach the envelope intact:\n%s", truncateForLog(env))
	}
	if strings.Contains(env, "…") || strings.Contains(env, "TRUNCATED") {
		t.Errorf("envelope should carry no truncation marker for a body under the seed cap:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeTruncatesOversizedDescription pins the seed ceiling
// (#666): a description over seedCap is marked truncated and points at the
// untruncated file - the ONE sanctioned truncation in the trigger path.
func TestBuildEnvelopeTruncatesOversizedDescription(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	body := strings.Repeat("x", seedCap+5000)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{Body: body}, 0), vetting.Grant{}, "", nil)

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
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK"})
	body := strings.Repeat("x", seedCap+5000)
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	env := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true, Body: body}, 0), vetting.Grant{}, "", nil)

	if !strings.Contains(env, "pull.json") {
		t.Errorf("a truncated PR description should point at pull.json:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeCommentDeletionVisibleInDelta pins #666's test case: a
// comment deleted between two dispatches is visible in the delta - computed
// by the existing snapshot diff (diffSnapshots), never GitHub's ?since=,
// which can express no signal for a deletion at all.
func TestBuildEnvelopeCommentDeletionVisibleInDelta(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	old := Snapshot{Comments: []snapshotComment{{ID: 1, User: "bob", Body: "will be deleted", CreatedAt: "t0"}}}
	cur := Snapshot{Comments: []snapshotComment{}}
	delta := diffSnapshots(old, cur, 0)
	gh := githubContext{snap: cur, delta: &delta}

	var issue issueCommentPayload
	issue.Issue.Number = 7

	env := ext.buildEnvelope(context.Background(), issue, "what changed?", gh, vetting.Grant{}, "", nil)

	if !strings.Contains(env, `deleted="1"`) {
		t.Errorf("envelope missing the deleted=1 comment-delta marker:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, "will be deleted") {
		t.Errorf("a deleted comment's last-known body should still be visible in the delta:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeTitleChangeMarking pins #666: an edited title re-seeds
// marked as changed, quoting the old value; an unchanged title is not
// re-marked.
func TestBuildEnvelopeTitleChangeMarking(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	var issue issueCommentPayload
	issue.Issue.Number = 7

	old := Snapshot{Title: "Old title"}
	cur := Snapshot{Title: "New title"}
	changed := diffSnapshots(old, cur, 0)
	gh := githubContext{snap: cur, delta: &changed}

	env := ext.buildEnvelope(context.Background(), issue, "task", gh, vetting.Grant{}, "", nil)
	if !strings.Contains(env, `New title (changed from "Old title")`) {
		t.Errorf("a changed title should be marked and quote the old value:\n%s", truncateForLog(env))
	}

	unchanged := diffSnapshots(cur, cur, 0)
	ghSame := githubContext{snap: cur, delta: &unchanged}
	envSame := ext.buildEnvelope(context.Background(), issue, "task", ghSame, vetting.Grant{}, "", nil)
	if strings.Contains(envSame, "changed from") {
		t.Errorf("an unchanged title should not be marked changed:\n%s", truncateForLog(envSame))
	}
}

// TestFilterGitHubJSONDropsNoiseKeepsShape pins #659's core promise: the
// filter is a DROP-LIST, never a rename - GitHub's own field names and
// nesting survive exactly (pull_request.head.ref stays put), while node_id,
// the *_url family, avatar_url and bare "url" are gone at EVERY nesting
// level, not just the top one.
func TestFilterGitHubJSONDropsNoiseKeepsShape(t *testing.T) {
	raw := []byte(`{
		"action":"opened",
		"pull_request":{
			"number":97,"node_id":"PR_abc","url":"https://api.github.com/x",
			"head":{"ref":"feat/x","sha":"abc123","repo":{"full_name":"acme/widgets"}},
			"base":{"ref":"main"},
			"user":{"login":"alice","avatar_url":"https://x","node_id":"U_1"}
		},
		"repository":{"full_name":"acme/widgets","html_url":"https://x"},
		"sender":{"login":"alice","avatar_url":"https://x"},
		"reactions":{"total_count":0},
		"performed_via_github_app":null
	}`)
	filtered := filterGitHubJSON(raw)

	var v map[string]any
	if err := json.Unmarshal([]byte(filtered), &v); err != nil {
		t.Fatalf("filtered output is not valid JSON: %v\n%s", err, filtered)
	}
	pr, ok := v["pull_request"].(map[string]any)
	if !ok {
		t.Fatalf("pull_request missing from filtered output:\n%s", filtered)
	}
	head, ok := pr["head"].(map[string]any)
	if !ok || head["ref"] != "feat/x" || head["sha"] != "abc123" {
		t.Errorf("pull_request.head.ref/sha not preserved with GitHub's own names/nesting:\n%s", filtered)
	}
	if base, ok := pr["base"].(map[string]any); !ok || base["ref"] != "main" {
		t.Errorf("pull_request.base.ref not preserved:\n%s", filtered)
	}
	for _, dropped := range []string{`"node_id"`, `"avatar_url"`, `"html_url"`, `"reactions"`, `"performed_via_github_app"`} {
		if strings.Contains(filtered, dropped) {
			t.Errorf("filtered event still contains dropped key %s:\n%s", dropped, filtered)
		}
	}
	// A bare "url" key is dropped too, everywhere - but "full_name" (which
	// merely CONTAINS no "url" substring at a word boundary) must survive.
	if _, ok := pr["url"]; ok {
		t.Errorf("pull_request.url should have been dropped:\n%s", filtered)
	}
	if repo, ok := v["repository"].(map[string]any); !ok || repo["full_name"] != "acme/widgets" {
		t.Errorf("repository.full_name not preserved:\n%s", filtered)
	}
}

// TestFilterGitHubJSONUnknownFieldSurvives pins the drop-list-not-allow-list
// promise: adding an unknown field to a fixture payload does not break the
// filter, and the field is NOT dropped (only the named drop-list is) - a
// drop-list needs no maintenance when GitHub adds a field.
func TestFilterGitHubJSONUnknownFieldSurvives(t *testing.T) {
	raw := []byte(`{"action":"opened","brand_new_field_from_github":"some value"}`)
	filtered := filterGitHubJSON(raw)
	if !strings.Contains(filtered, "brand_new_field_from_github") {
		t.Errorf("an unrecognised GitHub field should survive the drop-list filter unchanged:\n%s", filtered)
	}
}

// TestPermissionsTextMatchesGrant pins that <permissions> states EXACTLY the
// vocabulary Grant.allows checks (internal/vetting/grant.go) - the same
// tokens, so the envelope and enforcement can never name two different
// things.
func TestPermissionsTextMatchesGrant(t *testing.T) {
	for _, tt := range []struct {
		name string
		g    vetting.Grant
		want string
	}{
		{"review-only", vetting.Grant{PostReview: true, JoinPRConversation: true}, "post_review, join_pr_conversation"},
		{"issue-plan", vetting.Grant{JoinIssueConversation: true}, "join_issue_conversation"},
		{"fix", vetting.Grant{PushCommitsToPR: true, JoinPRConversation: true}, "join_pr_conversation, push_commits_to_pr"},
		{"none", vetting.Grant{}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := permissionsText(tt.g); got != tt.want {
				t.Errorf("permissionsText(%+v) = %q, want %q", tt.g, got, tt.want)
			}
		})
	}
}

// TestBuildEnvelopeReviewOnlyPermissionsNeverNameStagePR pins #659's test
// case 3: the envelope for a review-only run never grants open_pr /
// push_commits_to_pr - permissions state only what THIS run is allowed, not
// what a broader trigger on the same thread might be.
func TestBuildEnvelopeReviewOnlyPermissionsNeverNameStagePR(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	grant := vetting.Grant{PRScoped: true, PostReview: true, JoinPRConversation: true}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("unused"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pr.isLabelTrigger = true

	env := ext.buildEnvelope(context.Background(), pr, autoReviewTask, seedGC(Snapshot{IsPR: true}, 0), grant, "", nil)
	if strings.Contains(env, "open_pr") || strings.Contains(env, "push_commits_to_pr") {
		t.Errorf("review-only envelope must not grant PR-staging permissions:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, "post_review") {
		t.Errorf("review-only envelope missing its own granted permission:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeContextBlockListsWrittenFiles pins #660's wiring into the
// envelope: contextDirFiles labels each file WriteContextDir actually wrote
// with the endpoint that produced it, and buildEnvelope renders exactly that.
func TestBuildEnvelopeContextBlockListsWrittenFiles(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "issue.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comments.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	var issue issueCommentPayload
	issue.Issue.Number = 7

	files := contextDirFiles(dir, "acme", "widgets", 7, "")
	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), vetting.Grant{}, dir, files)

	if !strings.Contains(env, fmt.Sprintf("<context dir=%q>", dir)) {
		t.Errorf("envelope missing the context dir attribute:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, `<file name="issue.json">GET /repos/acme/widgets/issues/7</file>`) {
		t.Errorf("envelope missing the issue.json context file entry:\n%s", truncateForLog(env))
	}
	if !strings.Contains(env, `<file name="comments.json">GET /repos/acme/widgets/issues/7/comments</file>`) {
		t.Errorf("envelope missing the comments.json context file entry:\n%s", truncateForLog(env))
	}
}

// TestBuildEnvelopeNoContextBlockWithoutDir pins the degrade path: a run
// whose jail isn't wired (ctxDir == "") gets no <context> block at all,
// rather than an empty or malformed one.
func TestBuildEnvelopeNoContextBlockWithoutDir(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	var issue issueCommentPayload
	issue.Issue.Number = 7
	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), vetting.Grant{}, "", nil)
	if strings.Contains(env, "<context") {
		t.Errorf("envelope should have no <context> block when ctxDir is empty:\n%s", truncateForLog(env))
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

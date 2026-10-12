package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

func TestStripMentions(t *testing.T) {
	tests := []struct{ in, want string }{
		{"@alice please look", "alice please look"},
		{"cc @alice and @bob-smith.", "cc alice and bob-smith."},
		{"team @acme/core owns it", "team acme/core owns it"},
		{"mail me@example.com", "mail me@example.com"},
		{"see https://x.io/@alice", "see https://x.io/@alice"},
		{"inline `@Override` stays", "inline `@Override` stays"},
		{"```go\n@decorator\n```\n@alice", "```go\n@decorator\n```\nalice"},
		{"~~~\n@kept\n~~~", "~~~\n@kept\n~~~"},
		{"**@alice** wrote", "**alice** wrote"},
		{"", ""},
		// An unterminated fence (a truncated diff quote) must not disable stripping for the rest of the body,
		// or a real mention past it leaks through.
		{"```go\n@decorator\nno closing fence\n@alice", "```go\ndecorator\nno closing fence\nalice"},
		// An unterminated inline span (odd backtick count) must not read as
		// "code" for its trailing segment either - same fail-safe as fences.
		{"run `go test @alice", "run `go test alice"},
		// A closed inline span still protects only its own segment; a mention
		// outside it is stripped as usual.
		{"see `code` and @alice", "see `code` and alice"},
	}
	for _, tt := range tests {
		if got := stripMentions(tt.in); got != tt.want {
			t.Errorf("stripMentions(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAllChecksGreen(t *testing.T) {
	tests := []struct {
		name  string
		runs  []checkRunView
		green bool
	}{
		{"empty is not green", nil, false},
		{"one success", []checkRunView{{Status: "completed", Conclusion: "success"}}, true},
		{"neutral and skipped both count", []checkRunView{{Status: "completed", Conclusion: "neutral"}, {Status: "completed", Conclusion: "skipped"}}, true},
		{"still running", []checkRunView{{Status: "in_progress"}}, false},
		{"one failed among passing", []checkRunView{{Status: "completed", Conclusion: "success"}, {Status: "completed", Conclusion: "failure"}}, false},
	}
	for _, tt := range tests {
		if got := allChecksGreen(tt.runs); got != tt.green {
			t.Errorf("%s: allChecksGreen(...) = %v, want %v", tt.name, got, tt.green)
		}
	}
}

func TestBlockingHumanReviewer(t *testing.T) {
	tests := []struct {
		name    string
		reviews []prReview
		blocked bool
	}{
		{"no reviews", nil, false},
		{"approve only", []prReview{{User: ghUserRef{"bob"}, State: "APPROVED", CommitID: "h1"}}, false},
		{"standing request_changes", []prReview{{User: ghUserRef{"bob"}, State: "CHANGES_REQUESTED", CommitID: "h1"}}, true},
		{
			"later approval from the same reviewer clears it",
			[]prReview{
				{User: ghUserRef{"bob"}, State: "CHANGES_REQUESTED", CommitID: "h1", SubmittedAt: "2026-01-01T00:00:00Z"},
				{User: ghUserRef{"bob"}, State: "APPROVED", CommitID: "h1", SubmittedAt: "2026-01-02T00:00:00Z"},
			}, false,
		},
		{
			"dismissal clears it",
			[]prReview{{User: ghUserRef{"bob"}, State: "DISMISSED", CommitID: "h1"}}, false,
		},
		{
			"request_changes on a stale (pre-push) head does not block the new head",
			[]prReview{{User: ghUserRef{"bob"}, State: "CHANGES_REQUESTED", CommitID: "oldhead"}}, false,
		},
	}
	for _, tt := range tests {
		if _, blocked := blockingHumanReviewer(tt.reviews, "h1"); blocked != tt.blocked {
			t.Errorf("%s: blockingHumanReviewer(...) blocked = %v, want %v", tt.name, blocked, tt.blocked)
		}
	}
}

func TestAppendLineOnce(t *testing.T) {
	body, ok := appendLine("LGTM\n", "Merged as abc1234.")
	if !ok || body != "LGTM\n\nMerged as abc1234." {
		t.Fatalf("first append = %q, %v", body, ok)
	}
	if again, ok := appendLine(body, "Merged as abc1234."); ok || again != body {
		t.Errorf("second append changed the body: %q, %v", again, ok)
	}
	if body, _ := appendLine("", "x"); body != "x" {
		t.Errorf("empty body append = %q", body)
	}
}

// mergeSim is a mutable GitHub stub for the merge flow: tests flip reviews, check runs and head sha
// between webhook events, and it counts every outbound write by kind.
type mergeSim struct {
	mu            sync.Mutex
	reviews       string // GET .../reviews; BODY is replaced by body
	body          string // the quack review's current body - PUT edits persist here
	checks        string // GET .../check-runs
	suites        string // GET .../check-suites; "" behaves as zero suites (no CI on this head at all)
	issueComments string // GET .../comments; "" behaves as no own-PR comment markers
	headSHA       string
	labels        string // pullMeta's "labels" array, e.g. `[{"name":"quack:merge"}]`; "" behaves as no labels
	prState       string // the pull's "state"; "" behaves as "open"
	mergeErr      string // non-empty: PUT .../merge returns 405 with this message
	timeline      string // GET .../timeline; "" behaves as no matching events (known=false)
	timelineErr   bool   // true: GET .../timeline returns 500

	comments  atomic.Int32 // POST issue comments
	edits     atomic.Int32 // PUT review body
	merges    atomic.Int32
	reactions atomic.Int32
	lastEdit  atomic.Value // string
	lastMerge atomic.Value // string: request body
}

func (m *mergeSim) set(fn func(*mergeSim)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m)
}

func (m *mergeSim) server(t *testing.T) *httptest.Server {
	t.Helper()
	m.lastEdit.Store("")
	m.lastMerge.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		reviews, checks, head, mergeErr := strings.Replace(m.reviews, "BODY", m.body, 1), m.checks, m.headSHA, m.mergeErr
		suites, issueComments, labels := m.suites, m.issueComments, m.labels
		state := m.prState
		if state == "" {
			state = "open"
		}
		timeline, timelineErr := m.timeline, m.timelineErr
		m.mu.Unlock()
		if suites == "" {
			suites = `{"check_suites":[]}`
		}
		if issueComments == "" {
			issueComments = `[]`
		}
		if labels == "" {
			labels = `[]`
		}
		if timeline == "" {
			timeline = `[]`
		}
		switch {
		case strings.Contains(r.URL.Path, "/timeline"):
			if timelineErr {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			fmt.Fprint(w, timeline)
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			m.reactions.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/reviews/"):
			var edit struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&edit)
			m.edits.Add(1)
			m.lastEdit.Store(edit.Body)
			m.set(func(m *mergeSim) { m.body = edit.Body })
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, reviews)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			body, _ := io.ReadAll(r.Body)
			m.lastMerge.Store(string(body))
			if mergeErr != "" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				fmt.Fprintf(w, `{"message":%q}`, mergeErr)
				return
			}
			m.merges.Add(1)
			fmt.Fprint(w, `{"merged":true,"sha":"deadbeefcafe"}`)
		case strings.Contains(r.URL.Path, "/check-suites"):
			fmt.Fprint(w, suites)
		case strings.Contains(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, checks)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			m.comments.Add(1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, issueComments)
		case strings.HasSuffix(r.URL.Path, "/files"), strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprintf(w, `{"title":"Test PR","body":"","state":%q,"head":{"ref":"feature","sha":%q},"base":{"ref":"main"},"labels":%s}`, state, head, labels)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test PR","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	approvedOnHead1 = `[{"id":11,"state":"APPROVED","commit_id":"head1","body":"BODY","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"}]`
	checksEmpty     = `{"check_runs":[]}`
	checksPending   = `{"check_runs":[{"id":1,"name":"go-test","status":"in_progress"}]}`
	checksGreen     = `{"check_runs":[{"id":1,"name":"go-test","status":"completed","conclusion":"success"}]}`
	suitesNone      = `{"check_suites":[]}`
	suitesQueued    = `{"check_suites":[{"id":1,"status":"queued","app":{"slug":"github-actions"}}]}`
	suitesCompleted = `{"check_suites":[{"id":1,"status":"completed","conclusion":"success","app":{"slug":"github-actions"}}]}`
	suitesFailed    = `{"check_suites":[{"id":1,"status":"completed","conclusion":"failure","app":{"slug":"github-actions"}}]}`

	// approvedOnHead1PlusHumanChanges adds bob's standing CHANGES_REQUESTED
	// on the same head quack approved.
	approvedOnHead1PlusHumanChanges = `[
		{"id":11,"state":"APPROVED","commit_id":"head1","body":"LGTM","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"},
		{"id":12,"state":"CHANGES_REQUESTED","commit_id":"head1","body":"","user":{"login":"bob"},"submitted_at":"2026-01-02T00:00:00Z"}
	]`
	// approvedOnHead1PlusHumanReapproved is the same history with bob's later approval.
	approvedOnHead1PlusHumanReapproved = `[
		{"id":11,"state":"APPROVED","commit_id":"head1","body":"LGTM","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"},
		{"id":12,"state":"CHANGES_REQUESTED","commit_id":"head1","body":"","user":{"login":"bob"},"submitted_at":"2026-01-02T00:00:00Z"},
		{"id":13,"state":"APPROVED","commit_id":"head1","body":"","user":{"login":"bob"},"submitted_at":"2026-01-03T00:00:00Z"}
	]`
)

func checkEventBody(kind string) []byte {
	return []byte(fmt.Sprintf(`{"action":"completed",%q:{"head_sha":"head1","conclusion":"success","pull_requests":[{"number":7}]},
		"repository":{"name":"widgets","owner":{"login":"acme"}},"installation":{"id":5}}`, kind))
}

// settle gives the goroutines a webhook spawns time to finish their GitHub
// calls - used before asserting that nothing happened.
func settle() { time.Sleep(300 * time.Millisecond) }

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Approved head, checks still running when the label lands: no merge, no comment, one reaction.
// The check completing merges and appends the sha to the review, once.
func TestMergeWaitsForChecksThenMerges(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, body: "LGTM", checks: checksPending, headSHA: "head1"}
	srv := sim.server(t)
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", mergeLabelBody("alice")))
	waitFor(t, "the label ack", func() bool { return sim.reactions.Load() == 1 })
	settle()
	if sim.merges.Load() != 0 || sim.comments.Load() != 0 || sim.edits.Load() != 0 {
		t.Fatalf("pending checks: merges=%d comments=%d edits=%d; want all 0", sim.merges.Load(), sim.comments.Load(), sim.edits.Load())
	}
	if sim.reactions.Load() != 1 {
		t.Errorf("reactions = %d; want the single 👀 ack", sim.reactions.Load())
	}
	if len(fh.calls()) != 0 {
		t.Errorf("an approved-on-head PR must not be re-reviewed; dispatches = %d", len(fh.calls()))
	}

	sim.set(func(m *mergeSim) { m.checks = checksGreen })
	for _, kind := range []string{"check_run", "check_suite", "workflow_run"} {
		ext.handleWebhook(httptest.NewRecorder(), signedRequest(kind, checkEventBody(kind)))
	}
	// Wait for the edit, not just the merge: clearMergeIntent's store write
	// sits between them and can outlast a fixed sleep under load.
	waitFor(t, "the merge", func() bool { return sim.edits.Load() == 1 })
	settle()
	if sim.merges.Load() != 1 {
		t.Fatalf("merges = %d; want exactly 1 across three completion events", sim.merges.Load())
	}
	if !strings.Contains(sim.lastMerge.Load().(string), `"sha":"head1"`) {
		t.Errorf("merge body = %q; want it pinned to the reviewed head", sim.lastMerge.Load())
	}
	if sim.edits.Load() != 1 || sim.lastEdit.Load().(string) != "LGTM\n\nMerged as deadbee." {
		t.Errorf("edits=%d last=%q; want one review edit carrying the merge sha", sim.edits.Load(), sim.lastEdit.Load())
	}
	if sim.comments.Load() != 0 {
		t.Errorf("comments = %d; the outcome must land on the review, never as a comment", sim.comments.Load())
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7")); intent != nil {
		t.Errorf("intent = %+v; want it cleared after the merge", intent)
	}
}

// Label on a green, unreviewed PR dispatches a review and merges nothing; the approving review's event merges.
func TestMergeChecksGreenThenApproval(t *testing.T) {
	sim := &mergeSim{reviews: "[]", checks: checksGreen, headSHA: "head1"}
	srv := sim.server(t)
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", mergeLabelBody("alice")))
	fh.waitForDispatch(t, 2*time.Second)
	settle()
	if sim.merges.Load() != 0 || sim.comments.Load() != 0 {
		t.Fatalf("unreviewed: merges=%d comments=%d; want 0", sim.merges.Load(), sim.comments.Load())
	}

	sim.set(func(m *mergeSim) { m.reviews = approvedOnHead1 })
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request_review", pullRequestReviewBody("approved", "quack[bot]", 7)))
	// Wait for the edit, not just the merge - see TestMergeWaitsForChecksThenMerges.
	waitFor(t, "the merge", func() bool { return sim.edits.Load() == 1 })
	settle()
	if sim.merges.Load() != 1 || sim.edits.Load() != 1 || sim.comments.Load() != 0 {
		t.Errorf("merges=%d edits=%d comments=%d; want 1/1/0", sim.merges.Load(), sim.edits.Load(), sim.comments.Load())
	}
}

// An approval whose commit_id is no longer the head after a push must not merge, and stays silent.
func TestMergeApprovalOnOldHeadDoesNotMerge(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, checks: checksGreen, headSHA: "head2"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")
	if err := ext.store.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"action":"synchronize","number":7,"pull_request":{"head":{"sha":"head2"}},
		"repository":{"name":"widgets","owner":{"login":"acme"}},"installation":{"id":5},"sender":{"login":"alice"}}`)
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", body))
	for _, kind := range []string{"check_run", "check_suite"} {
		ext.handleWebhook(httptest.NewRecorder(), signedRequest(kind, checkEventBody(kind)))
	}
	settle()
	if sim.merges.Load() != 0 || sim.comments.Load() != 0 || sim.edits.Load() != 0 {
		t.Errorf("stale approval: merges=%d comments=%d edits=%d; want all 0", sim.merges.Load(), sim.comments.Load(), sim.edits.Load())
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent == nil {
		t.Error("intent must survive a stale approval so a fresh approval on the new head can still merge")
	}
}

// ownPRApprovalOnHead1 is an own-PR verdict in a plain issue comment (GitHub forbids self-review),
// so the reviewed head is pinned by the embedded head marker.
const ownPRApprovalOnHead1 = `[{"id":21,"body":"Own PR: verdict approve.\n\n<!-- quack:delivery:review:approve -->\n<!-- quack:delivery:head:head1 -->","user":{"login":"quack[bot]"},"created_at":"2026-01-01T00:00:00Z"}]`

// An own-PR verdict has no commit_id, so the embedded head marker must catch a push past the reviewed head.
func TestMergeOwnPRApprovalStaleAfterPushDoesNotMerge(t *testing.T) {
	sim := &mergeSim{reviews: "[]", issueComments: ownPRApprovalOnHead1, checks: checksGreen, headSHA: "head2"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")
	if err := ext.store.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"action":"synchronize","number":7,"pull_request":{"head":{"sha":"head2"}},
		"repository":{"name":"widgets","owner":{"login":"acme"}},"installation":{"id":5},"sender":{"login":"alice"}}`)
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", body))
	for _, kind := range []string{"check_run", "check_suite"} {
		ext.handleWebhook(httptest.NewRecorder(), signedRequest(kind, checkEventBody(kind)))
	}
	settle()
	if sim.merges.Load() != 0 {
		t.Errorf("merged a head the own-PR verdict never reviewed: merges=%d", sim.merges.Load())
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent == nil {
		t.Error("intent must survive a stale own-PR approval so a fresh one on the new head can still merge")
	}
}

// TestMergeRealFailureLandsOnReviewOnce: a conflict is appended to the
// approving review exactly once, even when several later events re-evaluate.
func TestMergeRealFailureLandsOnReviewOnce(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, body: "LGTM", checks: checksGreen, headSHA: "head1", mergeErr: "Pull Request has merge conflicts"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	if err := ext.store.SetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"), "alice"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		ext.handleWebhook(httptest.NewRecorder(), signedRequest("check_run", checkEventBody("check_run")))
	}
	waitFor(t, "the failure edit", func() bool { return sim.edits.Load() >= 1 })
	settle()
	if sim.merges.Load() != 0 || sim.comments.Load() != 0 {
		t.Errorf("merges=%d comments=%d; want 0/0", sim.merges.Load(), sim.comments.Load())
	}
	if sim.edits.Load() != 1 || sim.lastEdit.Load().(string) != "LGTM\n\nMerge failed: Pull Request has merge conflicts." {
		t.Errorf("edits=%d last=%q; want the conflict appended to the review exactly once", sim.edits.Load(), sim.lastEdit.Load())
	}

	// A pending required check reported by GitHub itself is not a failure.
	sim.set(func(m *mergeSim) { m.mergeErr = `Required status check "go-test" is in progress.` })
	before := sim.edits.Load()
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("check_run", checkEventBody("check_run")))
	settle()
	if sim.edits.Load() != before || sim.comments.Load() != 0 {
		t.Errorf("pending check must be silent: edits %d->%d comments=%d", before, sim.edits.Load(), sim.comments.Load())
	}
}

// TestMergeLabelRemovedWithdrawsIntent: unlabeling clears the standing
// authorization, so a later green+approved state does not merge.
func TestMergeLabelRemovedWithdrawsIntent(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, checks: checksGreen, headSHA: "head1"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")
	if err := ext.store.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(mergeLabelBody("alice")), `"labeled"`, `"unlabeled"`, 1)
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", []byte(body)))
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent != nil {
		t.Fatalf("intent = %+v; want it cleared on unlabel", intent)
	}
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("check_run", checkEventBody("check_run")))
	settle()
	if sim.merges.Load() != 0 {
		t.Errorf("merged without the label: merges=%d", sim.merges.Load())
	}
}

// `gh pr create --label quack:review --label quack:merge` delivers the merge label twice within a second;
// every trigger on that head while the review runs must post nothing and start no second review.
func TestMergeLabelTwiceWhileReviewRunsPostsNothing(t *testing.T) {
	sim := &mergeSim{reviews: "[]", checks: checksPending, headSHA: "head1"}
	srv := sim.server(t)
	ext, fh := newTestExtension(t, srv.URL, []string{"label", "merge"})

	review := strings.Replace(string(mergeLabelBody("alice")), "quack:merge", "quack-auto-review", 1)
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", []byte(review)))
	fh.waitForDispatch(t, 2*time.Second)
	for i := 0; i < 2; i++ {
		ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", mergeLabelBody("alice")))
	}
	waitFor(t, "both label acks", func() bool { return sim.reactions.Load() == 2 })
	settle()
	if sim.comments.Load() != 0 || sim.edits.Load() != 0 || sim.merges.Load() != 0 {
		t.Errorf("comments=%d edits=%d merges=%d; want all 0 while the review runs", sim.comments.Load(), sim.edits.Load(), sim.merges.Load())
	}
	if calls := fh.calls(); len(calls) != 1 {
		t.Errorf("dispatches = %d; want the one review the review label started", len(calls))
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7")); intent == nil || intent.RequestedBy != "alice" {
		t.Errorf("intent = %+v; want alice's standing authorization recorded", intent)
	}
}

// mergeLabelEvent builds one timeline event for mergeLabelActor's GET
// .../timeline stub - actor + whether it was a "labeled" or "unlabeled" event.
func mergeLabelEvent(actor string, labeled bool, createdAt string) string {
	action := "unlabeled"
	if labeled {
		action = "labeled"
	}
	return fmt.Sprintf(`{"event":%q,"created_at":%q,"actor":{"login":%q},"label":{"name":"quack:merge"}}`, action, createdAt, actor)
}

// A PR labeled quack:merge with no stored intent (missed delivery) adopts the label in tryMerge,
// but only once the timeline confirms an authorized human applied it.
func TestTryMergeAdoptsIntentFromLabelWhenNoneStored(t *testing.T) {
	sim := &mergeSim{
		reviews: "[]", checks: checksGreen, headSHA: "head1", labels: `[{"name":"quack:merge"}]`,
		timeline: "[" + mergeLabelEvent("alice", true, "2026-01-01T00:00:00Z") + "]",
	}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")

	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent != nil {
		t.Fatalf("intent = %+v; want none stored before the call", intent)
	}
	outcome, err := ext.tryMerge(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("tryMerge: %v", err)
	}
	if outcome != mergeUnreviewed {
		t.Errorf("outcome = %v; want mergeUnreviewed once the label is adopted and evaluation proceeds", outcome)
	}
	intent, _ := ext.store.GetMergeIntent(context.Background(), chatID)
	if intent == nil || intent.RequestedBy != "alice" {
		t.Errorf("intent = %+v; want it persisted from the timeline's actor", intent)
	}
}

// A PR closed after the intent and approval stood: tryMerge clears the intent and stops, never calling PUT /merge.
func TestTryMergeClosedPRDoesNotReachMerge(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, checks: checksGreen, headSHA: "head1", prState: "closed"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")
	if err := ext.store.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatal(err)
	}

	outcome, err := ext.tryMerge(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("tryMerge: %v", err)
	}
	if outcome != mergeNoIntent {
		t.Errorf("outcome = %v; want mergeNoIntent for a closed PR", outcome)
	}
	if sim.merges.Load() != 0 || sim.edits.Load() != 0 {
		t.Errorf("closed PR: merges=%d edits=%d; want 0/0", sim.merges.Load(), sim.edits.Load())
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent != nil {
		t.Errorf("intent = %+v; want it cleared once the PR is closed", intent)
	}
}

// Adoption refuses a bot actor, matching the delivery path's bot suffix check.
func TestTryMergeAdoptionRefusedForBotActor(t *testing.T) {
	sim := &mergeSim{
		reviews: "[]", checks: checksGreen, headSHA: "head1", labels: `[{"name":"quack:merge"}]`,
		timeline: "[" + mergeLabelEvent("some-app[bot]", true, "2026-01-01T00:00:00Z") + "]",
	}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")

	outcome, err := ext.tryMerge(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("tryMerge: %v", err)
	}
	if outcome != mergeNoIntent {
		t.Errorf("outcome = %v; want mergeNoIntent, refused for a bot actor", outcome)
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent != nil {
		t.Errorf("intent = %+v; want nothing persisted for a bot actor", intent)
	}
}

// Adoption refuses a human actor not in allowed_users, matching the labeled delivery's access check.
func TestTryMergeAdoptionRefusedForDisallowedActor(t *testing.T) {
	sim := &mergeSim{
		reviews: "[]", checks: checksGreen, headSHA: "head1", labels: `[{"name":"quack:merge"}]`,
		timeline: "[" + mergeLabelEvent("mallory", true, "2026-01-01T00:00:00Z") + "]",
	}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")

	outcome, err := ext.tryMerge(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("tryMerge: %v", err)
	}
	if outcome != mergeNoIntent {
		t.Errorf("outcome = %v; want mergeNoIntent, refused for an actor outside allowed_users", outcome)
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent != nil {
		t.Errorf("intent = %+v; want nothing persisted for a disallowed actor", intent)
	}
}

// The label is present but the timeline's latest labeled/unlabeled event says "unlabeled" (read skew):
// adoption trusts the timeline's most recent verdict over presence.
func TestTryMergeAdoptionRefusedWhenLatestEventIsUnlabeled(t *testing.T) {
	sim := &mergeSim{
		reviews: "[]", checks: checksGreen, headSHA: "head1", labels: `[{"name":"quack:merge"}]`,
		timeline: "[" +
			mergeLabelEvent("alice", true, "2026-01-01T00:00:00Z") + "," +
			mergeLabelEvent("alice", false, "2026-01-02T00:00:00Z") +
			"]",
	}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")

	outcome, err := ext.tryMerge(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("tryMerge: %v", err)
	}
	if outcome != mergeNoIntent {
		t.Errorf("outcome = %v; want mergeNoIntent, refused when the latest timeline event is unlabeled", outcome)
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent != nil {
		t.Errorf("intent = %+v; want nothing persisted when the latest event is unlabeled", intent)
	}
}

// A failing timeline lookup fails closed as mergeNoIntent, not an error: the label check is best-effort.
func TestTryMergeAdoptionRefusedWhenTimelineErrors(t *testing.T) {
	sim := &mergeSim{
		reviews: "[]", checks: checksGreen, headSHA: "head1", labels: `[{"name":"quack:merge"}]`,
		timelineErr: true,
	}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")

	outcome, err := ext.tryMerge(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("tryMerge: %v", err)
	}
	if outcome != mergeNoIntent {
		t.Errorf("outcome = %v; want mergeNoIntent, refused when the timeline call errors", outcome)
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent != nil {
		t.Errorf("intent = %+v; want nothing persisted when the timeline lookup fails", intent)
	}
}

// TestTryMergeNoIntentNoLabelStaysNoIntent: no stored intent and the label is
// absent from GitHub too - mergeNoIntent, and nothing gets persisted.
func TestTryMergeNoIntentNoLabelStaysNoIntent(t *testing.T) {
	sim := &mergeSim{reviews: "[]", checks: checksGreen, headSHA: "head1"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")

	outcome, err := ext.tryMerge(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("tryMerge: %v", err)
	}
	if outcome != mergeNoIntent {
		t.Errorf("outcome = %v; want mergeNoIntent", outcome)
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent != nil {
		t.Errorf("intent = %+v; want nothing persisted with no label and no prior intent", intent)
	}
}

// Near-simultaneous quack:review and quack:merge labeled events must both be handled: the merge-intent
// write must not wait on, or be dropped by, the racing review dispatch.
func TestConcurrentReviewAndMergeLabelDeliveriesBothHandled(t *testing.T) {
	sim := &mergeSim{reviews: "[]", checks: checksPending, headSHA: "head1"}
	srv := sim.server(t)
	ext, fh := newTestExtension(t, srv.URL, []string{"label", "merge"})

	review := strings.Replace(string(mergeLabelBody("alice")), "quack:merge", "quack-auto-review", 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", []byte(review)))
	}()
	go func() {
		defer wg.Done()
		ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", mergeLabelBody("alice")))
	}()
	wg.Wait()

	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	waitFor(t, "the merge intent", func() bool {
		intent, _ := ext.store.GetMergeIntent(context.Background(), chatID)
		return intent != nil
	})
	settle()

	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent == nil || intent.RequestedBy != "alice" {
		t.Errorf("intent = %+v; want alice's standing authorization recorded", intent)
	}
	if calls := fh.calls(); len(calls) != 1 {
		t.Errorf("dispatches = %d; want exactly one review dispatched", len(calls))
	}
}

func synchronizeBody(sha string) []byte {
	return []byte(fmt.Sprintf(`{"action":"synchronize","number":7,"pull_request":{"title":"Test PR","head":{"sha":%q}},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},"sender":{"login":"alice"}}`, sha))
}

// A push under a standing quack:merge intent re-reviews automatically instead of waiting for /review.
func TestSynchronizeUnderMergeIntentDispatchesReview(t *testing.T) {
	sim := &mergeSim{reviews: "[]", checks: checksGreen, headSHA: "head2"}
	srv := sim.server(t)
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")
	if err := ext.store.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatal(err)
	}

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", synchronizeBody("head2")))
	req := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(req.Ask.Message, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("dispatched envelope = %q; want the auto-review deliverable", req.Ask.Message)
	}
	if intent, _ := ext.store.GetMergeIntent(context.Background(), chatID); intent == nil || intent.DispatchedHead != "head2" {
		t.Errorf("intent = %+v; want dispatched_head recorded as head2", intent)
	}
}

// TestSynchronizeWithoutMergeIntentDispatchesNothing: a push on a PR with no
// standing quack:merge intent must not get an unsolicited re-review.
func TestSynchronizeWithoutMergeIntentDispatchesNothing(t *testing.T) {
	sim := &mergeSim{reviews: "[]", checks: checksGreen, headSHA: "head2"}
	srv := sim.server(t)
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", synchronizeBody("head2")))
	settle()
	if calls := fh.calls(); len(calls) != 0 {
		t.Errorf("dispatches = %d; want 0 with no standing intent", len(calls))
	}
}

// Two synchronize events for the same head dispatch once even after the first review completed:
// the intent's head-SHA guard, not inflight dedup, prevents the second.
func TestSynchronizeSameHeadTwiceDispatchesOnce(t *testing.T) {
	sim := &mergeSim{reviews: "[]", checks: checksGreen, headSHA: "head2"}
	srv := sim.server(t)
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})
	chatID := globalChatID("github-acme-widgets-7")
	if err := ext.store.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatal(err)
	}

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", synchronizeBody("head2")))
	fh.waitForDispatch(t, 2*time.Second)
	// Settle the first run so inflight dedup releases; only the head-SHA guard remains between the two events.
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "reviewed"})

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", synchronizeBody("head2")))
	settle()
	if calls := fh.calls(); len(calls) != 1 {
		t.Errorf("dispatches = %d; want exactly 1 across two synchronize events for the same head", len(calls))
	}
}

// An empty check-run list is not green: a review can land before any workflow queued. A queued suite means
// wait for the next completion event (no timer, no polling), unlike having no CI at all.
func TestMergeWaitsWhenNoChecksRegisteredYetThenMerges(t *testing.T) {
	sim := &mergeSim{reviews: "[]", body: "LGTM", checks: checksEmpty, suites: suitesQueued, headSHA: "head1"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	if err := ext.store.SetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"), "alice"); err != nil {
		t.Fatal(err)
	}

	sim.set(func(m *mergeSim) { m.reviews = approvedOnHead1 })
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request_review", pullRequestReviewBody("approved", "quack[bot]", 7)))
	settle()
	if sim.merges.Load() != 0 {
		t.Fatalf("merged with zero check runs registered on the head: merges=%d", sim.merges.Load())
	}

	sim.set(func(m *mergeSim) { m.checks = checksGreen })
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("check_run", checkEventBody("check_run")))
	waitFor(t, "the merge", func() bool { return sim.merges.Load() == 1 })
}

// Zero check runs and zero suites (docs-only PR, or no CI) never yield a completion event, so tryMerge
// merges right away and lets GitHub's required-check refusal be the last guard.
func TestMergeNoCheckSuitesMergesImmediately(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, body: "LGTM", checks: checksEmpty, suites: suitesNone, headSHA: "head1"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", mergeLabelBody("alice")))
	waitFor(t, "the merge", func() bool { return sim.merges.Load() == 1 })
}

// A queued suite with no runs yet: wait for the completion event, then merge once the run is green.
func TestMergeQueuedSuiteWaitsThenRunsCompleteMerges(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, body: "LGTM", checks: checksEmpty, suites: suitesQueued, headSHA: "head1"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", mergeLabelBody("alice")))
	settle()
	if sim.merges.Load() != 0 {
		t.Fatalf("merged with a queued suite and no runs reported: merges=%d", sim.merges.Load())
	}

	sim.set(func(m *mergeSim) { m.checks = checksGreen })
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("check_run", checkEventBody("check_run")))
	waitFor(t, "the merge", func() bool { return sim.merges.Load() == 1 })
}

// A suite already "completed" (success, no runs) emits no further event, so it must not count as pending;
// merge like the no-suite case.
func TestMergeCompletedSuiteNoRunsMergesImmediately(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, body: "LGTM", checks: checksEmpty, suites: suitesCompleted, headSHA: "head1"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})

	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request", mergeLabelBody("alice")))
	waitFor(t, "the merge", func() bool { return sim.merges.Load() == 1 })
}

// A suite completed with a failing conclusion and no runs is terminal too: GitHub would read the missing check
// as "expected" and stall, so tryMerge skips the merge and appends the reason once.
func TestMergeCompletedFailedSuiteNoRunsDoesNotMerge(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, body: "LGTM", checks: checksEmpty, suites: suitesFailed, headSHA: "head1"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	if err := ext.store.SetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"), "alice"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		ext.handleWebhook(httptest.NewRecorder(), signedRequest("check_run", checkEventBody("check_run")))
	}
	waitFor(t, "the failure edit", func() bool { return sim.edits.Load() >= 1 })
	settle()
	if sim.merges.Load() != 0 || sim.comments.Load() != 0 {
		t.Errorf("merges=%d comments=%d; want 0/0", sim.merges.Load(), sim.comments.Load())
	}
	if sim.edits.Load() != 1 || sim.lastEdit.Load().(string) != "LGTM\n\nMerge blocked: CI failed on this head and posted no check run to retry." {
		t.Errorf("edits=%d last=%q; want the failure appended to the review exactly once", sim.edits.Load(), sim.lastEdit.Load())
	}
}

// A human's standing CHANGES_REQUESTED on the head blocks an otherwise mergeable PR;
// the same reviewer re-approving on that head clears it.
func TestMergeBlockedByHumanRequestChangesThenReapprovalMerges(t *testing.T) {
	sim := &mergeSim{reviews: approvedOnHead1, body: "LGTM", checks: checksGreen, headSHA: "head1"}
	srv := sim.server(t)
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})
	if err := ext.store.SetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"), "alice"); err != nil {
		t.Fatal(err)
	}

	sim.set(func(m *mergeSim) { m.reviews = approvedOnHead1PlusHumanChanges })
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request_review", pullRequestReviewBody("changes_requested", "bob", 7)))
	settle()
	if sim.merges.Load() != 0 {
		t.Fatalf("merged despite bob's standing request_changes: merges=%d", sim.merges.Load())
	}

	sim.set(func(m *mergeSim) { m.reviews = approvedOnHead1PlusHumanReapproved })
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("pull_request_review", pullRequestReviewBody("approved", "bob", 7)))
	waitFor(t, "the merge", func() bool { return sim.merges.Load() == 1 })
}

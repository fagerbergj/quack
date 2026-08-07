package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

const testSecret = "shhh-webhook-secret"

// fakeRunner records the dispatched message and returns a fixed answer. Run's
// iterator optionally blocks on `block` (to prove the handler acks before the
// run finishes).
type fakeRunner struct {
	gotMessage   chan string
	gotSessionID chan string
	// gotCtx, when non-nil, receives the run ctx Run was called with - lets a
	// test inspect what dispatch stamped onto it (e.g. tools.GitHubSetupFromContext).
	gotCtx chan context.Context
	answer string
	block  chan struct{}
	calls  int32
	noPlan bool // when true, emit no dag_plan event (simulates a narration-only turn)
	// judgePassed simulates a node's trust gate clearing its judge round (a
	// node_done event with JudgePassed) - dispatch's fallback proxy for
	// "delivery was attempted" when nothing recorded an authoritative outcome
	// (see takeDeliveryResult).
	judgePassed bool
	// deliverOK/deliverErr simulate the trust gate's own commitDelivery
	// having ALREADY run and recorded its outcome (recordDeliveryResult) - the
	// authoritative signal dispatch prefers over judgePassed. deliverOK records
	// a success; deliverErr (mutually exclusive) records a failure.
	deliverOK  bool
	deliverErr string
	// deliverBranch, set alongside deliverErr, is the branch name a failure
	// comment should surface (#714) so the work is recoverable by hand.
	deliverBranch string
	// deliverReview simulates a run that DELIVERED A REVIEW specifically
	// (deliveryOutcome.reviewDelivered) - dispatch's ONLY trigger to advance
	// the review baseline (#459's incremental-review fix). Independent of
	// deliverOK: a plan/PR delivery must NOT set this.
	deliverReview bool
	// resets counts ResetSession calls - dispatch's T4 session-hygiene signal.
	resets int32
	// hitInput causes Run to emit a node_needs_input event mid-stream, simulating
	// an ask_user call by the worker - dispatch should post a HITQ comment instead
	// of the "produced no answer" tail.
	hitInput bool
	// revisedAnswer, if set, replaces answer starting on Run's SECOND call -
	// simulates the model fixing its answer on a mermaid-revise re-drive.
	revisedAnswer string
}

func (f *fakeRunner) ResetSession(context.Context, string, string) error {
	atomic.AddInt32(&f.resets, 1)
	return nil
}

func (f *fakeRunner) Run(ctx context.Context, _, sessionID, message string, _ []*genai.Part) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		calls := atomic.AddInt32(&f.calls, 1)
		if f.gotCtx != nil {
			select {
			case f.gotCtx <- ctx:
			default:
			}
		}
		if calls > 1 && f.revisedAnswer != "" {
			f.answer = f.revisedAnswer
		}
		if f.block != nil {
			// Also unblocks on ctx cancellation, simulating a run KILLED mid-flight
			// (as opposed to `block` closing, which simulates one finishing normally) -
			// needed by tests that cancel via the hub rather than closing block.
			select {
			case <-f.block:
			case <-ctx.Done():
				return
			}
		}
		if !f.noPlan {
			yield(stream.SSEEvent{Name: stream.EventDagPlan}, nil) // a real run executes a plan
		}
		if f.hitInput {
			yield(stream.NodeNeedsInput("node-1", "hitl-node-r0", "What version of Go should we target?"), nil)
		}
		if f.judgePassed {
			yield(stream.SSEEvent{Name: stream.EventNodeDone, Data: stream.NodeDoneData{JudgePassed: true}}, nil)
		}
		if f.deliverReview {
			recordDelivery(sessionID, deliveryOutcome{reviewDelivered: true})
		} else if f.deliverOK {
			recordDeliveryResult(sessionID, nil)
		} else if f.deliverErr != "" {
			recordDelivery(sessionID, deliveryOutcome{err: fmt.Errorf("%s", f.deliverErr), branch: f.deliverBranch})
		}
		select {
		case f.gotMessage <- message:
		default:
		}
		if f.gotSessionID != nil {
			select {
			case f.gotSessionID <- sessionID:
			default:
			}
		}
	}
}

func (f *fakeRunner) LatestAnswer(context.Context, string, string) string { return f.answer }

func (f *fakeRunner) PendingQuestion(context.Context, string, string) (string, bool) {
	return "", false
}

// planRunner is fakeRunner's dag_plan event with real DagPlanData (PlanID +
// one node), needed to assert the plan actually gets mirrored into the store -
// fakeRunner's zero-value Data can't round-trip through a real persist path.
type planRunner struct {
	gotMessage chan string
	answer     string
}

func (f *planRunner) Run(_ context.Context, _, _, message string, _ []*genai.Part) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		if !yield(stream.DagPlan("plan-1", []stream.DagNodeDef{{ID: "n1", Agent: "researcher"}}, nil), nil) {
			return
		}
		select {
		case f.gotMessage <- message:
		default:
		}
	}
}

func (f *planRunner) LatestAnswer(context.Context, string, string) string { return f.answer }
func (f *planRunner) ResetSession(context.Context, string, string) error  { return nil }
func (f *planRunner) PendingQuestion(context.Context, string, string) (string, bool) {
	return "", false
}

// stubGitHub serves the REST endpoints dispatch touches (installation resolve,
// token mint, comment post) and signals postedComment when a comment lands.
func stubGitHub(t *testing.T, postedComment chan<- string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated) // deterministic 👀 ack; ignored by these tests
			fmt.Fprint(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`) // changed-files list (snapshot fetch); overridden per-test where it matters
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`) // no prior review by default: first-time framing
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`) // PR commit list (snapshot fetch); overridden per-test where it matters
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			postedComment <- string(body)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`) // GET: review-comment / conversation list (snapshot fetch)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"A test issue.","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

// isIssueMetaPath reports whether path is a bare .../issues/{number} request
// (issueMeta's snapshot fetch) - as opposed to .../issues/{number}/comments
// or .../reactions, which the switch above already matches first.
func isIssueMetaPath(path string) bool {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "issues" {
		return false
	}
	_, err := strconv.Atoi(parts[len(parts)-1])
	return err == nil
}

func issueCommentBody(commentBody string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody))
}

// pullCommentBody is issueCommentBody but the issue is a pull request (GitHub
// marks PR comments with a non-null issue.pull_request).
func pullCommentBody(commentBody string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7,"pull_request":{}},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody))
}

func signedRequest(event string, body []byte) *http.Request {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, WebhookPath, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	return req
}

func newTestExtension(t *testing.T, runner Runner, apiBase string) *Extension {
	t.Helper()
	return newTestExtensionWithTriggers(t, runner, apiBase, nil, "")
}

// newTestExtensionWithTriggers builds an Extension with an explicit trigger
// set (nil/empty defaults to mention-only, matching applyDefaults) and label.
func newTestExtensionWithTriggers(t *testing.T, runner Runner, apiBase string, triggers []string, label string) *Extension {
	t.Helper()
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = apiBase
	return NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret:   testSecret,
		Mention:         "@quack",
		Triggers:        triggers,
		AutoReviewLabel: label,
		AllowedUsers:    []string{"alice"}, // every fixture's human invoker; see TestHandleWebhookInvokerAllowlist for the gate itself
	}, runner, nil, nil)
}

func pullRequestBody(action, labelName string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":%q,
		"number":7,
		"label":{"name":%q},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},
		"sender":{"login":"alice"}
	}`, action, labelName))
}

func TestHandleWebhookPROpenedTrigger(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		wantRun  bool
	}{
		{"pr_opened enabled fires", []string{"pr_opened"}, true},
		{"mention only is a no-op", []string{"mention"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 1)
			gh := stubGitHub(t, posted)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed"}
			ext := newTestExtensionWithTriggers(t, runner, gh.URL, tt.triggers, "")

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				select {
				case msg := <-runner.gotMessage:
					if !strings.Contains(msg, `<pull_request number="7">`) || !strings.Contains(msg, `"name":"widgets"`) {
						t.Errorf("run message missing the hoisted PR ask / repo event: %q", msg)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("pr_opened trigger did not dispatch a run")
				}
			} else {
				// fires is computed synchronously in handlePullRequest before any
				// goroutine is spawned - the decision is already final here.
				if atomic.LoadInt32(&runner.calls) != 0 {
					t.Error("pr_opened should not fire when only mention is configured")
				}
			}
		})
	}
}

func TestHandleWebhookLabelTrigger(t *testing.T) {
	tests := []struct {
		name      string
		triggers  []string
		labelName string
		wantRun   bool
	}{
		{"matching label + label trigger fires", []string{"label"}, "quack-auto-review", true},
		{"non-matching label is a no-op", []string{"label"}, "other-label", false},
		{"matching label but trigger not enabled is a no-op", []string{"mention"}, "quack-auto-review", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 1)
			gh := stubGitHub(t, posted)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed"}
			ext := newTestExtensionWithTriggers(t, runner, gh.URL, tt.triggers, "quack-auto-review")

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("labeled", tt.labelName)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				select {
				case <-runner.gotMessage:
				case <-time.After(2 * time.Second):
					t.Fatal("label trigger did not dispatch a run")
				}
			} else {
				// fires is computed synchronously in handlePullRequest before any
				// goroutine is spawned - the decision is already final here.
				if atomic.LoadInt32(&runner.calls) != 0 {
					t.Errorf("%s: should not have dispatched a run", tt.name)
				}
			}
		})
	}
}

func TestHandleWebhookAutoReviewUsesPRSessionID(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), gotSessionID: make(chan string, 1), answer: "reviewed"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"pr_opened"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case sessionID := <-runner.gotSessionID:
		if sessionID != "github-acme-widgets-7" {
			t.Errorf("session id = %q; want github-acme-widgets-7", sessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto-review did not dispatch a run")
	}
}

func TestHandleWebhookMentionRespectsTriggerSet(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		wantRun  bool
	}{
		{"mention enabled fires", []string{"mention"}, true},
		{"mention not configured is a no-op", []string{"pr_opened"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 1)
			gh := stubGitHub(t, posted)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "done"}
			ext := newTestExtensionWithTriggers(t, runner, gh.URL, tt.triggers, "")

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				select {
				case <-runner.gotMessage:
				case <-time.After(2 * time.Second):
					t.Fatal("mention trigger did not dispatch a run")
				}
			} else {
				// triggerTask checks e.triggers["mention"] synchronously before any
				// goroutine is spawned - the decision is already final here.
				if atomic.LoadInt32(&runner.calls) != 0 {
					t.Error("mention should not fire when not in the trigger set")
				}
			}
		})
	}
}

func TestHandleWebhookMentionTriggersRun(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "done - opened PR #12"}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case msg := <-runner.gotMessage:
		if !strings.Contains(msg, "add a feature") || !strings.Contains(msg, `"name":"widgets"`) {
			t.Errorf("run message missing task or repo: %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator was not invoked")
	}

	select {
	case body := <-posted:
		if !strings.Contains(body, "opened PR #12") {
			t.Errorf("posted comment missing answer: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
}

// TestTriggerTaskLineStart pins #656 test cases 4 and 5: the mention token
// must open a LINE, not just appear anywhere in the body - prose containing
// "quack" never dispatches, and a quoted "> /quack …" (GitHub's quote-reply
// markdown) never re-fires an earlier mention.
func TestTriggerTaskLineStart(t *testing.T) {
	ext := &Extension{mention: "/quack", triggers: map[string]bool{"mention": true}}
	tests := []struct {
		name     string
		body     string
		wantTask string
		wantOK   bool
	}{
		{"line-start token dispatches", "/quack address finding 1", "address finding 1", true},
		{"leading spaces still count as line start", "  /quack fix the typo", "fix the typo", true},
		{"prose containing the bare word does not dispatch", "quack's gate did not pass", "", false},
		{"prose mentioning the token mid-sentence does not dispatch", "please run /quack fix this", "", false},
		{"a quoted reply does not re-fire", "> /quack address finding 1\n\nlooks good", "", false},
		{"a quoted reply followed by a real request still dispatches from its own line", "> /quack old request\n\n/quack new request", "new request", true},
		{"a longer word sharing the prefix does not match", "/quackers is not a command", "", false},
		{"empty task after the token does not dispatch", "/quack", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := issueCommentPayload{Action: "created"}
			p.Comment.Body = tt.body
			task, ok := ext.triggerTask(p)
			if ok != tt.wantOK || task != tt.wantTask {
				t.Errorf("triggerTask(%q) = (%q, %v); want (%q, %v)", tt.body, task, ok, tt.wantTask, tt.wantOK)
			}
		})
	}
}

// pullRequestReviewBody is the pull_request_review webhook payload for a
// submitted review.
func pullRequestReviewBody(state, reviewer string, prNumber int) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"submitted",
		"review":{"state":%q,"user":{"login":%q}},
		"pull_request":{"number":%d},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, state, reviewer, prNumber))
}

// TestHandleWebhookRequestChangesEngagesOwnPR pins #656 test case 3 (closes
// #655): a request_changes review on a PR quack authored engages it to
// address the findings - authorship IS the flag, no label on the PR at all.
func TestHandleWebhookRequestChangesEngagesOwnPR(t *testing.T) {
	posted := make(chan string, 4)
	gh := stubFixGitHubFull(t, posted, nil, false, "", "quack[bot]") // no labels; PR authored by quack itself
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "addressed"}
	ext := newFixExtension(t, runner, gh.URL, newFixTestStore(t), "ci_fix")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request_review", pullRequestReviewBody("changes_requested", "alice", 7)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case msg := <-runner.gotMessage:
		for _, want := range []string{"requested changes", `"login":"alice"`} {
			if !strings.Contains(msg, want) {
				t.Errorf("engagement message missing %q: %q", want, msg)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request_changes review on quack's own PR did not engage it")
	}
}

// TestHandleWebhookRequestChangesIgnoresOtherPRs proves the label/mention
// triggers, not this path, still own a PR quack did NOT author - and an
// approving/commented review never engages regardless of authorship.
func TestHandleWebhookRequestChangesIgnoresOtherPRs(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		prAuthorLogin string
	}{
		{"not quack's PR", "changes_requested", "someone-else"},
		{"quack's PR but an approval, not changes requested", "approved", "quack[bot]"},
		{"quack's PR but a plain comment review", "commented", "quack[bot]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 4)
			gh := stubFixGitHubFull(t, posted, nil, false, "", tt.prAuthorLogin)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "addressed"}
			ext := newFixExtension(t, runner, gh.URL, newFixTestStore(t), "ci_fix")

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request_review", pullRequestReviewBody(tt.state, "alice", 7)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			// Authorship resolves async (an HTTP round trip); bound the wait and
			// fail immediately if it fires.
			select {
			case <-runner.gotMessage:
				t.Error("must not engage")
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

// A webhook-dispatched run must persist a turn + DAG-carrying chat_events for
// its session, exactly like a UI-initiated run - otherwise getChat/
// GetTurnsWithContent has nothing to assemble and the GitHub tab shows an empty
// session even though the run actually executed a plan (issue: DAG rows existed
// but 0 turns/chat_events, so the UI rendered nothing).
func TestHandleWebhookPersistsTurnAndEventsForUI(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// planRunner emits a real dag_plan (PlanID + a node), unlike fakeRunner's
	// empty-Data placeholder, so this test can assert the plan was actually
	// mirrored into the store for getChat to find.
	runner := &planRunner{gotMessage: make(chan string, 1), answer: "reviewed"}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL
	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret,
		Mention:       "@quack",
		AllowedUsers:  []string{"alice"},
	}, runner, st, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack review this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back; dispatch never completed")
	}

	sessionID := "github-acme-widgets-7"
	// SaveDagPlan (like the REST handler's) persists off the run's hot path in a
	// bare goroutine, so it can still be in flight the instant the comment posts;
	// poll briefly rather than racing it.
	var turns []store.TurnContent
	deadline := time.Now().Add(2 * time.Second)
	for {
		turns, err = st.GetTurnsWithContent(context.Background(), "quack", "alice", sessionID)
		if err != nil {
			t.Fatalf("GetTurnsWithContent: %v", err)
		}
		if len(turns) == 1 && turns[0].Plan != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turns = %+v; want 1 turn with a DAG plan attached (the webhook dispatch must persist a turn + dag_plan like a UI-initiated run)", turns)
		}
		time.Sleep(10 * time.Millisecond)
	}

	events, err := st.LoadChatEvents(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("LoadChatEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("chat_events is empty; a github-dispatched run must durably persist its SSE stream like runChat does")
	}
}

// A webhook run that answers WITHOUT executing a plan (a narration preamble like
// "Let me start by cloning the repo…") is nudged exactly once to actually run the
// work - the fix for reviews that posted the preamble as if it were the review.
func TestHandleWebhookNudgesWhenNoPlanRan(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	// A label trigger is work by construction - nudge it once if it produced no plan.
	runner := &fakeRunner{gotMessage: make(chan string, 4), answer: "reviewed", noPlan: true}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"issue_implement"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-posted: // wait for dispatch to finish
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
	if got := atomic.LoadInt32(&runner.calls); got != 2 {
		t.Errorf("runner invoked %d times, want 2 (initial run + one nudge when no plan ran)", got)
	}

	// Control: a run that DOES execute a plan is not nudged.
	posted2 := make(chan string, 1)
	gh2 := stubGitHub(t, posted2)
	defer gh2.Close()
	planned := &fakeRunner{gotMessage: make(chan string, 4), answer: "reviewed"} // noPlan=false ⇒ emits dag_plan
	ext2 := newTestExtensionWithTriggers(t, planned, gh2.URL, []string{"issue_implement"}, "")
	rec2 := httptest.NewRecorder()
	ext2.handleWebhook(rec2, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	select {
	case <-posted2:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back (control)")
	}
	if got := atomic.LoadInt32(&planned.calls); got != 1 {
		t.Errorf("runner invoked %d times, want 1 (a plan ran ⇒ no nudge)", got)
	}

	// Control: a MENTION that produced no plan is never nudged - this is the
	// regression the old work-verb regex caused (a quoted method call like
	// `it.migrate(connection)` armed the nudge and forced a re-review that
	// discarded the reply already written).
	posted3 := make(chan string, 1)
	gh3 := stubGitHub(t, posted3)
	defer gh3.Close()
	mentionRunner := &fakeRunner{gotMessage: make(chan string, 4), answer: "reviewed", noPlan: true}
	ext3 := newTestExtension(t, mentionRunner, gh3.URL)
	rec3 := httptest.NewRecorder()
	ext3.handleWebhook(rec3, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	select {
	case <-posted3:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back (mention control)")
	}
	if got := atomic.LoadInt32(&mentionRunner.calls); got != 1 {
		t.Errorf("runner invoked %d times, want 1 (a mention is never nudged)", got)
	}
}

// TestHandleWebhookNoAnswerFailsLoudly guards #568: a run that neither hits
// its deadline nor gets cancelled, but persists no final answer, must post an
// explicit failure - not the old "quack finished but produced no answer."
// placeholder, which read identically to a run that legitimately had nothing
// to say. The comment must also tell the maintainer what to do next, like the
// deadline path already does.
func TestHandleWebhookNoAnswerFailsLoudly(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: ""}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack summarize this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	var body string
	select {
	case body = <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}

	if strings.Contains(body, "quack finished but produced no answer.") {
		t.Errorf("posted the old silent placeholder verbatim: %q", body)
	}
	if !strings.Contains(body, "Re-apply the label to retry") {
		t.Errorf("comment does not say what to do next: %q", body)
	}
	if !strings.Contains(strings.ToLower(body), "no error") {
		t.Errorf("comment does not describe what actually happened: %q", body)
	}
}

// dispatch posts whatever the run produced verbatim: an invalid ```mermaid
// block is a deterministic gate criterion (vetting.mermaidCriterion) that
// fails the node and feeds the error back to the worker as revise feedback,
// not something stripped here.
//
// mermaidValidateTestTimeout bounds the two tests below, which exercise the
// real mermaid.js validator up to 3 times end-to-end (~1-1.5s each cold).
const mermaidValidateTestTimeout = 10 * time.Second

func TestHandleWebhookInvalidMermaidRevisedFixesDiagram(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	invalid := "Here's the plan:\n\n```mermaid\nA[Start] --> B[Finish]\n```\n\nDone."
	fixed := "Here's the plan:\n\n```mermaid\nflowchart TD\n    A[Start] --> B[Finish]\n```\n\nDone."
	gotMessage := make(chan string, 2)
	runner := &fakeRunner{gotMessage: gotMessage, answer: invalid, revisedAnswer: fixed}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	// First Run() call is the original dispatch; the second is the mermaid
	// revise nudge - it must name the concrete parse error. mermaidValidateTimeout
	// (not 2s): each vetting.FindInvalidMermaid/DegradeInvalidMermaid call now
	// shells out to the real mermaid.js parser (#574) - a cold node+jsdom+mermaid
	// import measures ~1-1.5s on its own, and this path calls it twice.
	<-gotMessage
	var nudge string
	select {
	case nudge = <-gotMessage:
	case <-time.After(mermaidValidateTestTimeout):
		t.Fatal("no revise nudge dispatched")
	}
	if !strings.Contains(nudge, "invalid mermaid") || !strings.Contains(nudge, "parse error") {
		t.Fatalf("nudge = %q, want it to name the invalid mermaid and the concrete parse error", nudge)
	}

	var body string
	select {
	case body = <-posted:
	case <-time.After(mermaidValidateTestTimeout):
		t.Fatal("no comment posted back")
	}
	if !strings.Contains(body, "```mermaid") || strings.Contains(body, "```text") {
		t.Fatalf("posted comment = %s, want the fixed diagram posted as mermaid, not degraded", body)
	}
	if atomic.LoadInt32(&runner.calls) != 2 {
		t.Fatalf("Run() calls = %d, want exactly 2 (original + one bounded revise)", runner.calls)
	}
}

// When the agent can't fix it even after the one revise round, the ceiling
// degrades to a VISIBLE, labeled ```text block - never a silent strip.
func TestHandleWebhookInvalidMermaidStillBadAfterReviseDegradesVisibly(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	answer := "Here's the plan:\n\n```mermaid\nA[Start] --> B[Finish]\n```\n\nDone."
	runner := &fakeRunner{gotMessage: make(chan string, 2), answer: answer}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	var body string
	select {
	case body = <-posted:
	case <-time.After(mermaidValidateTestTimeout):
		t.Fatal("no comment posted back")
	}
	if strings.Contains(body, "```mermaid") {
		t.Fatalf("posted comment = %s, want the still-invalid diagram degraded, not shipped as mermaid", body)
	}
	if !strings.Contains(body, "```text") || !strings.Contains(body, "A[Start]") || !strings.Contains(body, "B[Finish]") {
		t.Fatalf("posted comment = %s, want the diagram content still VISIBLE inside a labeled text block", body)
	}
	if !strings.Contains(body, "invalid mermaid diagram") {
		t.Fatalf("posted comment = %s, want a visible note explaining the degradation", body)
	}
	if atomic.LoadInt32(&runner.calls) != 2 {
		t.Fatalf("Run() calls = %d, want exactly 2 (original + one bounded revise, no loop)", runner.calls)
	}
}

// When the run submits a formal review (github_submit_review), the review IS the
// deliverable on the PR - dispatch must NOT also post the run's text summary as a
// duplicate top-level comment.
func TestHandleWebhookSubmittedReviewSkipsSummaryComment(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "I reviewed it.", judgePassed: true, deliverOK: true}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"mention"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack review this PR")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	// The run must have been driven (message delivered) …
	select {
	case <-runner.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("run was not dispatched")
	}
	// … but NO summary comment posted (a formal review was submitted).
	select {
	case body := <-posted:
		t.Errorf("a duplicate summary comment was posted despite a submitted review: %q", body)
	case <-time.After(300 * time.Millisecond):
	}
}

// A FAILED delivery must not count as delivered: a github_pull_request whose
// result carries an error (e.g. the run was killed before the branch was pushed)
// previously suppressed the summary/failure comment on the CALL alone - a silent
// death with a "delivered" log line and nothing on GitHub (#286). It must also
// NOT fall back to the worker's own self-reported answer, which can claim
// success it never had (#714) - the comment must be the actual delivery error.
func TestHandleWebhookFailedDeliveryStillComments(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "partial progress",
		judgePassed: true, deliverErr: "github_pull_request: branch not pushed", deliverBranch: "quack/issue-66"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"mention"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack implement this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-runner.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("run was not dispatched")
	}
	select {
	case body := <-posted:
		if strings.Contains(body, "partial progress") {
			t.Errorf("failure comment = %q; must not use the worker's own self-report", body)
		}
		if !strings.Contains(body, "branch not pushed") {
			t.Errorf("failure comment = %q; want the delivery error", body)
		}
		if !strings.Contains(body, "quack/issue-66") {
			t.Errorf("failure comment = %q; want the branch name so the work is recoverable", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted after a FAILED delivery - the silent-death bug")
	}
}

// Two runs on the SAME PR session are deduped: the second trigger finds the
// sessionID in the inflight set and returns early, dropped rather than queued.
// After the first run completes, a third dispatch succeeds.
// waitInflightClear blocks until the dedup claim for sessionID is released (the
// dispatch goroutine's deferred inflight.Delete has run), failing after 2s.
func waitInflightClear(t *testing.T, ext *Extension, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, held := ext.inflight.Load(sessionID); !held {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("inflight claim for %s never cleared", sessionID)
}

// staysAt polls got() every 5ms across dur, failing the instant it leaves
// want - bound-proves a negative when nothing will ever signal completion.
func staysAt(t *testing.T, dur time.Duration, want int32, got func() int32) {
	t.Helper()
	deadline := time.Now().Add(dur)
	for {
		if v := got(); v != want {
			t.Fatalf("value = %d during the wait window; want it to stay %d", v, want)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDispatchSerializesSameSession(t *testing.T) {
	posted := make(chan string, 2)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 4), answer: "ok", block: make(chan struct{})}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"mention"}, "")

	// Two mentions on issue #7 → same session. Both ack 202; dedup drops the second.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
	}

	// Wait for the first run to be in flight (it increments calls, then blocks).
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&runner.calls) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Run() itself is blocked on f.block, so it can't signal completion - poll instead.
	staysAt(t, 150*time.Millisecond, 1, func() int32 { return atomic.LoadInt32(&runner.calls) })

	close(runner.block) // let the first finish

	// Wait for posted comment to confirm dispatch completed cleanly.
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never posted its answer")
	}

	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Errorf("runner.calls = %d after first dispatch and deduped second; want 1", got)
	}

	// The inflight claim is released by dispatch's deferred Delete, which runs on
	// RETURN - i.e. AFTER the comment post above. <-posted alone doesn't imply the
	// session is free, so wait for the claim to actually clear before re-triggering
	// (otherwise the third dispatch races the release and gets deduped in CI).
	waitInflightClear(t, ext, "github-acme-widgets-7")

	// After the first completes, a third dispatch on the same sessionID must succeed.
	rec3 := httptest.NewRecorder()
	ext.handleWebhook(rec3, signedRequest("issue_comment", issueCommentBody("@quack again")))
	if rec3.Code != http.StatusAccepted {
		t.Fatalf("third dispatch status = %d; want 202", rec3.Code)
	}

	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("third dispatch never posted its answer")
	}

	if got := atomic.LoadInt32(&runner.calls); got != 2 {
		t.Errorf("runner.calls = %d after third dispatch; want 2 (first ran, second was deduped, third ran)", got)
	}
}

func TestHandleWebhookNoMentionIsNoop(t *testing.T) {
	runner := &fakeRunner{gotMessage: make(chan string, 1)}
	ext := newTestExtension(t, runner, "http://unused")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("just chatting, no mention")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	// triggerTask decides synchronously, before any goroutine is spawned.
	if atomic.LoadInt32(&runner.calls) != 0 {
		t.Error("orchestrator should not run without a mention")
	}
}

func TestHandleWebhookUnhandledEventIsNoop(t *testing.T) {
	runner := &fakeRunner{gotMessage: make(chan string, 1)}
	ext := newTestExtension(t, runner, "http://unused")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("star", []byte(`{"action":"created"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	// handleWebhook's default case decides synchronously, before any goroutine
	// is spawned.
	if atomic.LoadInt32(&runner.calls) != 0 {
		t.Error("orchestrator should not run for an unhandled event")
	}
}

func TestHandleWebhookBadSignature(t *testing.T) {
	runner := &fakeRunner{gotMessage: make(chan string, 1)}
	ext := newTestExtension(t, runner, "http://unused")

	body := issueCommentBody("@quack do it")
	req := signedRequest("issue_comment", body)
	// Tamper with the body AFTER signing so the signature no longer matches.
	req.Body = io.NopCloser(bytes.NewReader(append(body, ' ')))

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	// verifySignature is checked synchronously before any goroutine is spawned.
	if atomic.LoadInt32(&runner.calls) != 0 {
		t.Error("a bad signature must not trigger a run")
	}
}

func TestHandleWebhookAcksBeforeRunFinishes(t *testing.T) {
	runner := &fakeRunner{gotMessage: make(chan string, 1), block: make(chan struct{})}
	ext := newTestExtension(t, runner, "http://unused")

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack slow task")))
		close(done)
	}()
	select {
	case <-done:
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler blocked on the run instead of acking fast")
	}
	close(runner.block) // let the (blocked) dispatch goroutine finish
}

func TestHandleWebhookMentionPostsEyesReaction(t *testing.T) {
	reacted := make(chan string, 1) // path + body of the reaction POST
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
			reacted <- r.URL.Path + " " + string(body)
		default:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "done"}
	ext := newTestExtension(t, runner, srv.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack take a look")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case got := <-reacted:
		if !strings.Contains(got, "/repos/acme/widgets/issues/comments/999/reactions") {
			t.Errorf("reaction hit wrong endpoint: %q", got)
		}
		if !strings.Contains(got, `"content":"eyes"`) {
			t.Errorf("reaction content should be eyes; got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no 👀 reaction was posted on the mention")
	}
}

func TestAckReactionFailureDoesNotBlockRun(t *testing.T) {
	posted := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			http.Error(w, "boom", http.StatusInternalServerError) // reaction fails
		case strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "handled"}
	ext := newTestExtension(t, runner, srv.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack do it")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	// The run must still be dispatched and its answer posted despite the failed reaction.
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("a failed 👀 reaction blocked the run dispatch")
	}
}

// seedGC builds a first-load githubContext from a snapshot - buildEnvelope's
// most common test fixture (no store, no prior snapshot to diff against, so
// delta stays nil and the envelope seeds everything).
func seedGC(snap Snapshot, excludeCommentID int64) githubContext {
	_ = excludeCommentID // exclusion is now the caller's issueCommentPayload.Comment.ID, read by commentsBlock
	return githubContext{snap: snap, firstLoad: true}
}

// fakeIntentClassifier is a fixed-verdict IntentClassifier double: tests set
// verdict directly instead of tuning prose to trip a regex. errAlways
// simulates the classifier failing outright. The three prompts it answers
// (isWorkRequest's WORK/CONVERSATIONAL, classifyPRDeliverable's
// REVIEW/COMMIT, and classifyIssueDeliverable's IMPLEMENT/COMMENT) are
// distinguished by content; deliverable/deliverableErr and
// issueDeliverable/issueDeliverableErr let a test degrade one classifier
// independently of the others.
type fakeIntentClassifier struct {
	verdict             string // "WORK" or "CONVERSATIONAL", or any other/blank to test the unparseable path
	deliverable         string // "REVIEW" or "COMMIT", or any other/blank to test the unparseable path
	issueDeliverable    string // "IMPLEMENT" or "COMMENT", or any other/blank to test the unparseable path
	errAlways           error
	deliverableErr      error
	issueDeliverableErr error
	calls               int32
}

func (f *fakeIntentClassifier) Classify(_ context.Context, prompt string) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.errAlways != nil {
		return "", f.errAlways
	}
	if strings.Contains(prompt, "REVIEW or COMMIT") {
		if f.deliverableErr != nil {
			return "", f.deliverableErr
		}
		return f.deliverable, nil
	}
	if strings.Contains(prompt, "IMPLEMENT or COMMENT") {
		if f.issueDeliverableErr != nil {
			return "", f.issueDeliverableErr
		}
		return f.issueDeliverable, nil
	}
	return f.verdict, nil
}

// TestBuildEnvelopeDeliverableClassification pins the classification
// buildEnvelope keeps from the old prose builder: a PR mention with genuine
// review intent gets the review deliverable; one with implement intent
// (vetting.ImplementationIntent) gets the generic implement deliverable; a
// plain issue mention never mentions review at all.
func TestBuildEnvelopeDeliverableClassification(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK"})

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this, focusing on the auth path"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this, focusing on the auth path", seedGC(Snapshot{IsPR: true}, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(env, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("review-intent PR envelope missing the review deliverable:\n%s", env)
	}
	if !strings.Contains(env, `<pull_request number="7">`) {
		t.Errorf("envelope missing the hoisted pull_request ask block:\n%s", env)
	}

	// A PR request that DOES ask to change code gets the implement deliverable.
	implEnv := ext.buildEnvelope(context.Background(), pr, "fix the null dereference in the auth path and open a PR", seedGC(Snapshot{IsPR: true}, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(implEnv, "<deliverable>a commit addressing the requested change</deliverable>") {
		t.Errorf("implement-intent PR envelope missing the implement deliverable:\n%s", implEnv)
	}

	// A non-PR issue mention never mentions review, and hoists <issue> not <pull_request>.
	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack add a feature"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	imsg := ext.buildEnvelope(context.Background(), issue, "add a feature", seedGC(Snapshot{}, 0), vetting.Grant{}, "", nil)
	if strings.Contains(imsg, "a review with inline comments") {
		t.Errorf("issue envelope should not mention the review deliverable:\n%s", imsg)
	}
	if !strings.Contains(imsg, `<issue number="7">`) {
		t.Errorf("issue envelope missing the hoisted issue ask block:\n%s", imsg)
	}
}

// TestBuildEnvelopeDeliverableClassifierResolvesFindingsAddress pins #689's
// exact production failure: "please address these findings" has no delivery
// word and its impl verb isn't clause-initial, so
// vetting.ImplementationIntent misreads it as review-only. With both
// post_review and push_commits_to_pr granted (the real ledger's permission
// set) the classifier - not the regex - picks the deliverable, and gets this
// one right.
func TestBuildEnvelopeDeliverableClassifierResolvesFindingsAddress(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK", deliverable: "COMMIT"})
	grant := vetting.Grant{PRScoped: true, PostReview: true, PushCommitsToPR: true}

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack please address these findings make sure they are valid first"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "please address these findings make sure they are valid first", seedGC(Snapshot{IsPR: true}, 0), grant, "", nil)
	if !strings.Contains(env, "<deliverable>a commit addressing the requested change</deliverable>") {
		t.Errorf("a findings-address request with push_commits_to_pr granted should get the commit deliverable, not a second review:\n%s", env)
	}

	// Same grant, a genuine review ask still gets the review deliverable.
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK", deliverable: "REVIEW"})
	revEnv := ext.buildEnvelope(context.Background(), pr, "take another look at the auth changes", seedGC(Snapshot{IsPR: true}, 0), grant, "", nil)
	if !strings.Contains(revEnv, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("a genuine review ask should still get the review deliverable:\n%s", revEnv)
	}
}

// TestBuildEnvelopeDeliverableBoundedBySoleGrant pins #689's case 3: when the
// grant permits only ONE of review/commit, that's the deliverable regardless
// of what the message reads like - the classifier is never even consulted
// (nothing to choose between), so it cannot hand back an ungranted plan.
func TestBuildEnvelopeDeliverableBoundedBySoleGrant(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	classifier := &fakeIntentClassifier{verdict: "WORK", deliverable: "COMMIT"} // even a COMMIT verdict must not surface without push_commits_to_pr
	ext.SetIntentClassifier(classifier)
	grant := vetting.Grant{PRScoped: true, PostReview: true, PushCommitsToPR: false}

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack please address these findings"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "please address these findings", seedGC(Snapshot{IsPR: true}, 0), grant, "", nil)
	if !strings.Contains(env, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("with only post_review granted, the deliverable must fall back to review even though the message asks for a fix:\n%s", env)
	}
	// One call total: isWorkRequest's WORK/CONVERSATIONAL check. The
	// deliverable prompt is never sent - there was nothing to choose between.
	if calls := atomic.LoadInt32(&classifier.calls); calls != 1 {
		t.Errorf("classifier called %d times, want 1 (deliverable choice is bounded to the sole grant, no model call needed)", calls)
	}
}

// TestBuildEnvelopeDeliverableClassifierFailureFallsBack pins #689's case 4:
// a classifier error (or timeout, same path) falls back to
// vetting.ImplementationIntent, exactly as if no classifier were wired at
// all - a hung/erroring model must not stall or corrupt the deliverable
// choice.
func TestBuildEnvelopeDeliverableClassifierFailureFallsBack(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK", deliverableErr: errors.New("model unavailable")})
	grant := vetting.Grant{PRScoped: true, PostReview: true, PushCommitsToPR: true}

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack please address these findings"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "please address these findings", seedGC(Snapshot{IsPR: true}, 0), grant, "", nil)
	// vetting.ImplementationIntent("please address these findings") is false (no
	// delivery word) - the same regex-driven review deliverable a classifier
	// failure must fall back to, not silently drop into panic or default-commit.
	if !strings.Contains(env, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("classifier failure should fall back to vetting.ImplementationIntent's reading:\n%s", env)
	}
}

// TestBuildEnvelopeIssueDeliverableClassification pins #713: an issue comment
// asking for implementation gets the PR deliverable when open_pr is granted
// (quack:implement present), but the same comment without that grant stays
// bounded to a plain reply - the label decides what's LEGAL, the message
// decides what's ASKED.
func TestBuildEnvelopeIssueDeliverableClassification(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{issueDeliverable: "IMPLEMENT"})

	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack implement this and open the PR"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	granted := vetting.Grant{OpenPR: true}
	env := ext.buildEnvelope(context.Background(), issue, "implement this and open the PR", seedGC(Snapshot{}, 0), granted, "", nil)
	if !strings.Contains(env, "a pull request implementing the approved plan") {
		t.Errorf("implement request with open_pr granted should get the PR deliverable:\n%s", env)
	}

	// Same message, no quack:implement label: the grant bounds it back to a comment.
	ungranted := ext.buildEnvelope(context.Background(), issue, "implement this and open the PR", seedGC(Snapshot{}, 0), vetting.Grant{}, "", nil)
	if strings.Contains(ungranted, "a pull request implementing") {
		t.Errorf("implement request WITHOUT open_pr granted must not surface the PR deliverable:\n%s", ungranted)
	}
	if !strings.Contains(ungranted, "an answer to their message") {
		t.Errorf("ungranted implement request should fall back to the comment deliverable:\n%s", ungranted)
	}

	// A plain question with the label still present stays a comment - the
	// classifier, not the grant alone, decides what was actually asked.
	ext.SetIntentClassifier(&fakeIntentClassifier{issueDeliverable: "COMMENT"})
	question := ext.buildEnvelope(context.Background(), issue, "what do you think the right approach is here?", seedGC(Snapshot{}, 0), granted, "", nil)
	if !strings.Contains(question, "an answer to their message") {
		t.Errorf("a plain question should stay a comment even with open_pr granted:\n%s", question)
	}
}

// TestBuildEnvelopeIssueDeliverableClassifierFailureFallsBack pins #713's
// robustness requirement: a classifier failure (error, timeout, or
// unparseable answer) must fall back to vetting.ImplementationIntent's
// wording heuristic, never straight to conversational - a cold classifier
// silently downgrading an implementation request inverted a real run.
func TestBuildEnvelopeIssueDeliverableClassifierFailureFallsBack(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{issueDeliverableErr: errors.New("model unavailable")})
	granted := vetting.Grant{OpenPR: true}

	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack implement this, commit it, and open a PR"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// vetting.ImplementationIntent("implement this, commit it, and open a PR") is
	// true (implement verb + delivery word) - the classifier failure must still
	// land on the PR deliverable via the heuristic, not silently downgrade it.
	env := ext.buildEnvelope(context.Background(), issue, "implement this, commit it, and open a PR", seedGC(Snapshot{}, 0), granted, "", nil)
	if !strings.Contains(env, "a pull request implementing the approved plan") {
		t.Errorf("classifier failure should fall back to vetting.ImplementationIntent's reading, not conversational:\n%s", env)
	}

	// A message with no delivery wording falls back to the heuristic's negative reading too.
	plain := ext.buildEnvelope(context.Background(), issue, "what do you think?", seedGC(Snapshot{}, 0), granted, "", nil)
	if !strings.Contains(plain, "an answer to their message") {
		t.Errorf("classifier failure on a non-implement message should still fall back to the comment deliverable:\n%s", plain)
	}
}

// TestBuildEnvelopeSeedsFullOnFirstLoad pins the seed half of #666's session
// model: session creation seeds the whole comment thread as <comments
// count="N">, triggering comment excluded (it's already inside the <event>
// block's own comment.body).
func TestBuildEnvelopeSeedsFullOnFirstLoad(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	var issue issueCommentPayload
	issue.Issue.Number = 269
	issue.Issue.Title = "Evaluate mem0"
	issue.Comment.ID = 999 // the triggering comment

	snap := Snapshot{
		Body: "We should evaluate mem0 as a memory backend.",
		Comments: []snapshotComment{
			{ID: 100, User: "hegu-1", Body: "The gate should stay the authority.", CreatedAt: "t0"},
			{ID: 200, User: "quack-jason[bot]", Body: "# Implementation Plan: mem0 as a vector store", CreatedAt: "t1"},
			{ID: 999, User: "fagerbergj", Body: "rework it - mem0 is not a store", CreatedAt: "t2"},
		},
	}
	env := ext.buildEnvelope(context.Background(), issue, "rework it - mem0 is not a store", seedGC(snap, issue.Comment.ID), vetting.Grant{}, "", nil)
	if !strings.Contains(env, "evaluate mem0 as a memory backend") {
		t.Errorf("envelope missing the seeded issue body:\n%s", env)
	}
	if !strings.Contains(env, `<comments count="2">`) {
		t.Errorf("envelope missing the full first-load comment seed (2, excluding the trigger):\n%s", env)
	}
	if strings.Contains(env, "hegu-1") == false || strings.Contains(env, "Implementation Plan: mem0 as a vector store") == false {
		t.Errorf("envelope missing seeded comment content:\n%s", env)
	}
	// The triggering comment is quoted once, inside the event block - not
	// duplicated into the seeded comments array too.
	if n := strings.Count(env, "rework it - mem0 is not a store"); n != 0 {
		t.Errorf("triggering comment should not appear in the seeded comments array (n=%d):\n%s", n, env)
	}
}

// TestBuildEnvelopeResumeSeedsOnlyDelta pins the resume half of #666: a
// later run seeds only what changed - new/edited/deleted comments - never
// the whole thread again.
func TestBuildEnvelopeResumeSeedsOnlyDelta(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	var issue issueCommentPayload
	issue.Issue.Number = 7

	old := Snapshot{Body: "desc", Comments: []snapshotComment{{ID: 1, User: "bob", Body: "first comment", CreatedAt: "t0"}}}
	cur := Snapshot{Body: "desc", Comments: []snapshotComment{
		{ID: 1, User: "bob", Body: "first comment", CreatedAt: "t0"},
		{ID: 2, User: "carol", Body: "a brand new comment", CreatedAt: "t1"},
	}}
	delta := diffSnapshots(old, cur, 0)
	env := ext.buildEnvelope(context.Background(), issue, "what's new?", githubContext{snap: cur, delta: &delta}, vetting.Grant{}, "", nil)
	if !strings.Contains(env, "a brand new comment") {
		t.Errorf("resume envelope missing the new comment:\n%s", env)
	}
	if strings.Contains(env, "first comment") {
		t.Errorf("resume envelope re-injected an UNCHANGED comment (should only carry the delta):\n%s", env)
	}
	if !strings.Contains(env, `<comments new="1" edited="0" deleted="0">`) {
		t.Errorf("resume envelope missing the delta attributes:\n%s", env)
	}

	// An unchanged snapshot: the delta is empty, nothing extra is injected.
	unchanged := diffSnapshots(cur, cur, 0)
	if !unchanged.Empty() {
		t.Fatalf("diffSnapshots(cur, cur) = %+v; want an empty delta", unchanged)
	}
	noopEnv := ext.buildEnvelope(context.Background(), issue, "anything new?", githubContext{snap: cur, delta: &unchanged}, vetting.Grant{}, "", nil)
	if strings.Contains(noopEnv, "a brand new comment") || strings.Contains(noopEnv, "first comment") {
		t.Errorf("an unchanged-snapshot resume should inject no comment content:\n%s", noopEnv)
	}
}

// TestBuildEnvelopeChangedFilesOnPRRuns pins the scope note: <changed_files>
// is seeded on PR runs only, with GitHub's own filename/additions/deletions
// shape (no reshaping needed - changedFile already matches pulls/{n}/files
// field-for-field).
func TestBuildEnvelopeChangedFilesOnPRRuns(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK"})
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	snap := Snapshot{
		IsPR:  true,
		Files: []changedFile{{Filename: "a.go", Additions: 10, Deletions: 2}, {Filename: "b.go", Additions: 1}},
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(snap, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(env, `<changed_files count="2" additions="11" deletions="2">`) {
		t.Errorf("envelope missing the changed_files summary attributes:\n%s", env)
	}
	if !strings.Contains(env, `"filename":"a.go"`) || !strings.Contains(env, `"additions":10`) {
		t.Errorf("envelope missing per-file churn in GitHub's own field names:\n%s", env)
	}

	var issue issueCommentPayload
	issue.Issue.Number = 7
	issueEnv := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), vetting.Grant{}, "", nil)
	if strings.Contains(issueEnv, "<changed_files") {
		t.Errorf("an issue-scoped envelope should carry no changed_files block:\n%s", issueEnv)
	}
}

// TestBuildEnvelopeIncrementalReviewScoping pins #459 §5 under the envelope:
// a resume with new commits gets the "what's new" deliverable; a resume with
// none says a full review is not owed either.
func TestBuildEnvelopeIncrementalReviewScoping(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK"})
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// First-time review: no prior baseline, the full-review deliverable.
	first := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true}, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(first, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("first-time review should get the full-review deliverable:\n%s", first)
	}

	// Resume with new commits: the incremental deliverable, naming the SHA.
	withNew := ext.buildEnvelope(context.Background(), pr, "review this", githubContext{
		snap:       Snapshot{IsPR: true},
		newCommits: []snapshotCommit{{SHA: "abc1234567", Message: "fix the bug"}},
	}, vetting.Grant{}, "", nil)
	if !strings.Contains(withNew, "a review of what is new since the last one") || !strings.Contains(withNew, "abc1234") {
		t.Errorf("incremental review envelope missing the scoped deliverable naming the new commit:\n%s", withNew)
	}

	// Resume with zero new commits still reads as "scoped to what's new" (a
	// review baseline exists), not the first-time framing.
	noneNew := ext.buildEnvelope(context.Background(), pr, "review this", githubContext{snap: Snapshot{IsPR: true}, newCommits: []snapshotCommit{}}, vetting.Grant{}, "", nil)
	if !strings.Contains(noneNew, "already looked at every commit") {
		t.Errorf("zero-new-commits resume should say there's nothing new, not the first-time framing:\n%s", noneNew)
	}
}

// TestBuildEnvelopeConversationalFollowup pins that a PR mention classified
// CONVERSATIONAL gets the reply deliverable, never review/implement
// language - and a genuine work request still gets the work deliverable.
func TestBuildEnvelopeConversationalFollowup(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack which finding matters most? No need to re-review."), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "which finding matters most? No need to re-review.", seedGC(Snapshot{IsPR: true}, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(env, "<deliverable>a reply to their message, posted as a comment") {
		t.Errorf("conversational envelope missing the reply deliverable:\n%s", env)
	}

	// A genuine review request, classified as a work request, gets the work deliverable.
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK"})
	rev := ext.buildEnvelope(context.Background(), pr, "please review this PR", seedGC(Snapshot{IsPR: true, HeadRef: "x"}, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(rev, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("a classified work request must still get the review deliverable:\n%s", rev)
	}
}

// TestBuildEnvelopeMentionClassifiedAsWork/Conversational pin that the
// classifier's verdict (not task wording) decides the deliverable.
func TestBuildEnvelopeMentionClassifiedAsWork(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "WORK"})
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this, focusing on the auth path"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this, focusing on the auth path", seedGC(Snapshot{IsPR: true}, 0), vetting.Grant{}, "", nil)
	if strings.Contains(env, "a reply to their message") {
		t.Errorf("a mention classified WORK should not get the conversational deliverable:\n%s", env)
	}
}

func TestBuildEnvelopeMentionClassifiedAsConversational(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(&fakeIntentClassifier{verdict: "CONVERSATIONAL"})
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack what did you mean by that finding?"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "what did you mean by that finding?", seedGC(Snapshot{IsPR: true}, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(env, "a reply to their message") {
		t.Errorf("a mention classified CONVERSATIONAL should get the reply deliverable:\n%s", env)
	}
}

// TestBuildEnvelopeLabelTriggerNeverClassifies pins rule 1: a label trigger
// is work by construction, so buildEnvelope must never call the classifier
// for it - not even to double-check.
func TestBuildEnvelopeLabelTriggerNeverClassifies(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	classifier := &fakeIntentClassifier{verdict: "CONVERSATIONAL"} // even a "no" verdict must not flip a label trigger
	ext.SetIntentClassifier(classifier)

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pr.isLabelTrigger = true
	env := ext.buildEnvelope(context.Background(), pr, autoReviewTask, seedGC(Snapshot{IsPR: true}, 0), vetting.Grant{}, "", nil)
	if strings.Contains(env, "a reply to their message") {
		t.Errorf("a label-triggered PR request should never get the conversational deliverable:\n%s", env)
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 0 {
		t.Errorf("classifier called %d times for a label trigger, want 0 (work by construction)", calls)
	}
}

// TestBuildEnvelopePartialFixOmitsClosesKeyword pins the partial-fix
// deliverable distinction: quack:partial-fix suppresses the Closes keyword
// language, read off the FRESHLY FETCHED snapshot labels (gh.snap.Labels),
// never a separately-threaded flag.
func TestBuildEnvelopePartialFixOmitsClosesKeyword(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.labels.PartialFix = "quack:partial-fix"
	var issue issueCommentPayload
	issue.Issue.Number = 42
	issue.isLabelTrigger = true

	full := ext.buildEnvelope(context.Background(), issue, "implement it", seedGC(Snapshot{Labels: []string{"quack:implement"}}, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(full, "Closes #42") {
		t.Errorf("a non-partial implement envelope should ask for a Closes keyword:\n%s", full)
	}

	partial := ext.buildEnvelope(context.Background(), issue, "implement it", seedGC(Snapshot{Labels: []string{"quack:implement", "quack:partial-fix"}}, 0), vetting.Grant{}, "", nil)
	if strings.Contains(partial, "Closes #42") {
		t.Errorf("a partial-fix envelope must not ask for a Closes keyword:\n%s", partial)
	}
	if !strings.Contains(partial, "partial fix") {
		t.Errorf("a partial-fix envelope should say so:\n%s", partial)
	}
}

// TestBuildEnvelopePlanOnlyDeliverable pins the plan-only deliverable and
// that the issue body appears exactly once (planTask never embeds it; only
// the hoisted <issue><description> does - #619's duplicate-body defect).
func TestBuildEnvelopePlanOnlyDeliverable(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	const body = "Widgets are refetched on every request."
	up := issuesPayload{}
	up.Issue.Number = 7
	up.Issue.Title = "Add widget cache"
	up.Issue.Body = body
	task := planTask(up)

	var synthetic issueCommentPayload
	synthetic.Issue.Number = 7
	synthetic.Comment.User.Login = "alice"
	synthetic.Repository.Name = "widgets"
	synthetic.Repository.Owner.Login = "acme"
	synthetic.planOnly = true
	synthetic.isLabelTrigger = true

	env := ext.buildEnvelope(context.Background(), synthetic, task, seedGC(Snapshot{Body: body}, 0), vetting.Grant{}, "", nil)

	if n := strings.Count(env, body); n != 1 {
		t.Errorf("issue body appears %d times in the plan-only envelope, want exactly 1:\n%s", n, env)
	}
	if !strings.Contains(env, "PLANNING-ONLY") || !strings.Contains(env, "ANSWER TEXT is the plan") {
		t.Errorf("plan-only envelope missing the plan deliverable:\n%s", env)
	}
	for _, banned := range []string{"git_push", "github_pull_request", "create a branch"} {
		if strings.Contains(env, banned) {
			t.Errorf("plan-only envelope contains delivery instruction %q:\n%s", banned, env)
		}
	}
}

// TestIsWorkRequestTolerantOfWrappedVerdict: a small instruct model rarely
// answers with a bare word. Exact matching made "**WORK**" unparseable, which
// fails safe to conversational - so every genuine "@quack review this" would
// have quietly lost the review framing. CONVERSATIONAL must win when both
// appear, since "WORK" is a substring of neither but a hedged answer can name
// both ("not WORK, CONVERSATIONAL").
func TestIsWorkRequestTolerantOfWrappedVerdict(t *testing.T) {
	for _, tt := range []struct {
		answer string
		want   bool
	}{
		{"WORK", true},
		{"**WORK**", true},
		{"WORK.", true},
		{" work \n", true},
		{"CONVERSATIONAL", false},
		{"**CONVERSATIONAL**", false},
		{"not WORK, CONVERSATIONAL", false},
		{"I am unable to classify this", false},
	} {
		t.Run(tt.answer, func(t *testing.T) {
			ext := newTestExtension(t, &fakeRunner{}, "http://unused")
			ext.SetIntentClassifier(&fakeIntentClassifier{verdict: tt.answer})
			if got := ext.isWorkRequest(context.Background(), "@quack review this"); got != tt.want {
				t.Errorf("isWorkRequest(%q) = %v, want %v", tt.answer, got, tt.want)
			}
		})
	}
}

func TestIsWorkRequestFailsSafe(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")

	cases := []struct {
		name       string
		classifier IntentClassifier
	}{
		{"nil classifier", nil},
		{"classifier error", &fakeIntentClassifier{errAlways: errors.New("model unavailable")}},
		{"unparseable answer", &fakeIntentClassifier{verdict: "maybe?"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ext.SetIntentClassifier(c.classifier)
			if ext.isWorkRequest(context.Background(), "review this PR") {
				t.Errorf("isWorkRequest = true, want false (fail safe to conversational)")
			}
		})
	}
}

// blockingIntentClassifier blocks until its ctx is done, then reports the
// ctx's error - simulating a classifier call that hangs past its deadline.
type blockingIntentClassifier struct{}

func (blockingIntentClassifier) Classify(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// TestIsWorkRequestTimeoutFailsSafe pins the timeout bound: a classifier call
// that hangs past its deadline fails safe to conversational, not work.
func TestIsWorkRequestTimeoutFailsSafe(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	ext.SetIntentClassifier(blockingIntentClassifier{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if ext.isWorkRequest(ctx, "review this PR") {
		t.Error("isWorkRequest = true on timeout, want false (fail safe to conversational)")
	}
}

// TestBuildEnvelopeQuotedCodeCorrectionNotWorkRequest is the regression test
// for the bug this classifier replaced: workVerbRe read a method call quoted
// inside code (it.migrate(connection)) as the imperative "migrate", which
// armed the no-plan nudge and forced a whole re-review that discarded the
// reply the model had already written. A real model should call this
// CONVERSATIONAL (a correction, not an instruction); this pins that the
// deliverable follows the classifier's verdict end to end.
func TestBuildEnvelopeQuotedCodeCorrectionNotWorkRequest(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	classifier := &fakeIntentClassifier{verdict: "CONVERSATIONAL"}
	ext.SetIntentClassifier(classifier)

	task := "That finding was wrong - it.migrate(connection) is called during setup, not teardown."
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), vetting.Grant{}, "", nil)
	if !strings.Contains(env, "a reply to their message") {
		t.Errorf("a correction quoting code must be conversational, not a work request:\n%s", env)
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 1 {
		t.Errorf("classifier called %d times, want exactly 1 for a mention", calls)
	}
}

// TestDispatchFirstLoadSeedsThenResumeInjectsDelta is the end-to-end version
// of #459: dispatch #1 on a fresh session seeds the FULL context (no prior
// snapshot); a comment is added on GitHub between runs; dispatch #2 (a
// resume) injects ONLY that new comment, not the whole thread again. Uses a
// real store so the snapshot persistence itself is exercised, not just the
// in-memory diff function.
func TestDispatchFirstLoadSeedsThenResumeInjectsDelta(t *testing.T) {
	posted := make(chan string, 1) // first dispatch's posted answer
	var commentsJSON atomic.Value
	commentsJSON.Store(`[{"id":1,"body":"the original comment","user":{"login":"bob"},"updated_at":"t0"}]`)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			select {
			case posted <- string(body):
			default:
			}
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON.Load().(string))
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Evaluate widgets","body":"Should we use widgets?","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL
	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "ok"}
	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret, Mention: "@quack", AllowedUsers: []string{"alice"},
	}, runner, st, nil)

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack what do you think?")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}
	var firstMsg string
	select {
	case firstMsg = <-runner.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never reached the orchestrator")
	}
	if !strings.Contains(firstMsg, "the original comment") {
		t.Errorf("first (seed) dispatch missing the existing comment:\n%s", firstMsg)
	}
	if !strings.Contains(firstMsg, "Should we use widgets?") {
		t.Errorf("first (seed) dispatch missing the issue body:\n%s", firstMsg)
	}

	// Wait for dispatch #1 to fully finish (not just post) so the inflight entry
	// is cleaned up before dispatch #2, or #2 races the release and gets deduped.
	waitInflightClear(t, ext, "github-acme-widgets-7")

	// A new comment lands on GitHub between runs.
	commentsJSON.Store(`[
		{"id":1,"body":"the original comment","user":{"login":"bob"},"updated_at":"t0"},
		{"id":2,"body":"a brand-new follow-up","user":{"login":"carol"},"updated_at":"t1"}
	]`)

	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack anything new?")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second dispatch status = %d; want 202", rec2.Code)
	}
	var secondMsg string
	select {
	case secondMsg = <-runner.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("second (resume) dispatch never reached the orchestrator")
	}
	if !strings.Contains(secondMsg, "a brand-new follow-up") {
		t.Errorf("resume dispatch missing the new comment:\n%s", secondMsg)
	}
	if strings.Contains(secondMsg, "the original comment") {
		t.Errorf("resume dispatch re-injected the UNCHANGED comment - should carry only the delta:\n%s", secondMsg)
	}
}

// TestKilledRunPreservesWatermarkDelta pins that the conversation watermark
// advances on run COMPLETION, not at dispatch: a run cancelled mid-flight
// (hub.CancelRun) must NOT persist the snapshot it fetched, so the delta it
// never acted on survives for the next trigger.
//
// Three dispatches, one store: #1 completes and establishes the watermark at
// c1. #2 is killed mid-flight after c2 lands - c2 must NOT be marked seen.
// #3 must see BOTH c2 and c3, proving the killed run's delta was never
// consumed.
func TestKilledRunPreservesWatermarkDelta(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[{"id":1,"body":"first comment","user":{"login":"bob"},"updated_at":"t0"}]`)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON.Load().(string))
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Evaluate widgets","body":"Should we use widgets?","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL

	const sessionID = "github-acme-widgets-7"
	newExt := func(runner *fakeRunner) *Extension {
		return NewExtension(app, config.GitHubExtensionConfig{
			WebhookSecret: testSecret, Mention: "@quack", AllowedUsers: []string{"alice"},
		}, runner, st, nil)
	}

	// #1: baseline dispatch, completes normally, persists the watermark at
	// "first comment".
	runner1 := &fakeRunner{gotMessage: make(chan string, 1), answer: "ok"}
	ext1 := newExt(runner1)
	rec1 := httptest.NewRecorder()
	ext1.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack hello")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("dispatch #1 status = %d; want 202", rec1.Code)
	}
	select {
	case <-runner1.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch #1 never reached the orchestrator")
	}
	waitInflightClear(t, ext1, sessionID)

	// c2 lands.
	commentsJSON.Store(`[
		{"id":1,"body":"first comment","user":{"login":"bob"},"updated_at":"t0"},
		{"id":2,"body":"second comment landed","user":{"login":"carol"},"updated_at":"t1"}
	]`)

	// #2: killed mid-flight (cancelled via the hub, the same mechanism
	// DELETE/stop and a superseding run use) before it can persist anything.
	runner2 := &fakeRunner{gotMessage: make(chan string, 1), answer: "ok", block: make(chan struct{})}
	ext2 := newExt(runner2)
	rec2 := httptest.NewRecorder()
	ext2.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack anything new?")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("dispatch #2 status = %d; want 202", rec2.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&runner2.calls) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&runner2.calls); got != 1 {
		t.Fatalf("dispatch #2 never started its run (calls=%d)", got)
	}
	if !ext2.hub.CancelRun(sessionID) {
		t.Fatal("could not cancel dispatch #2's run - was it not registered?")
	}
	waitInflightClear(t, ext2, sessionID)

	// c3 lands.
	commentsJSON.Store(`[
		{"id":1,"body":"first comment","user":{"login":"bob"},"updated_at":"t0"},
		{"id":2,"body":"second comment landed","user":{"login":"carol"},"updated_at":"t1"},
		{"id":3,"body":"third comment landed","user":{"login":"dave"},"updated_at":"t2"}
	]`)

	// #3: completes normally. Its delta must contain BOTH c2 (which #2 never
	// got to mark as seen) and c3.
	runner3 := &fakeRunner{gotMessage: make(chan string, 1), answer: "ok"}
	ext3 := newExt(runner3)
	rec3 := httptest.NewRecorder()
	ext3.handleWebhook(rec3, signedRequest("issue_comment", issueCommentBody("@quack anything new now?")))
	if rec3.Code != http.StatusAccepted {
		t.Fatalf("dispatch #3 status = %d; want 202", rec3.Code)
	}
	var msg3 string
	select {
	case msg3 = <-runner3.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch #3 never reached the orchestrator")
	}
	if !strings.Contains(msg3, "second comment landed") {
		t.Errorf("dispatch #2's delta was lost - the watermark advanced before it completed:\n%s", msg3)
	}
	if !strings.Contains(msg3, "third comment landed") {
		t.Errorf("dispatch #3 missing the newest comment:\n%s", msg3)
	}
}

// TestReviewBaselineDecoupledFromGeneralSnapshot is the coordinator-flagged
// fix for #459/#460: the review scope (gh.newCommits) must be keyed off the
// commits quack actually DELIVERED a review at, never off the general
// snapshot (which advances on every dispatch, review or not). Scenario:
// review delivered at [c1] -> c2 pushed -> a CONVERSATIONAL dispatch lands
// (advances the general snapshot to [c1,c2] but must NOT advance the review
// baseline) -> a review request must still see c2 as new -> once that review
// IS delivered, the baseline advances and the NEXT review sees zero new.
func TestReviewBaselineDecoupledFromGeneralSnapshot(t *testing.T) {
	// Two synthetic commits with real, distinct git patch-ids (gitPatchID
	// reads a diff from stdin - no clone needed, see snapshot.go).
	diffs := map[string]string{
		"c1": "diff --git a/f1.txt b/f1.txt\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/f1.txt\n@@ -0,0 +1 @@\n+c1\n",
		"c2": "diff --git a/f2.txt b/f2.txt\nnew file mode 100644\nindex 0000000..2222222\n--- /dev/null\n+++ b/f2.txt\n@@ -0,0 +1 @@\n+c2\n",
	}
	var commitsJSON atomic.Value
	commitsJSON.Store(`[{"sha":"c1","commit":{"message":"add f1"}}]`)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app": // botLogin, called computing this run's permission grant (#662)
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case strings.Contains(r.URL.Path, "/pulls/") && strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, commitsJSON.Load().(string))
		case strings.Contains(r.URL.Path, "/commits/"): // single-commit diff (Accept: v3.diff)
			parts := strings.Split(r.URL.Path, "/")
			sha := parts[len(parts)-1]
			fmt.Fprint(w, diffs[sha])
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"","state":"open","head":{"ref":"feature","sha":"headsha"},"base":{"ref":"main"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL

	run := func(runner *fakeRunner, task string, verdict string) string {
		t.Helper()
		ext := NewExtension(app, config.GitHubExtensionConfig{
			WebhookSecret: testSecret, Mention: "@quack", AllowedUsers: []string{"alice"},
		}, runner, st, nil)
		ext.SetIntentClassifier(&fakeIntentClassifier{verdict: verdict})
		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack "+task)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch status = %d; want 202 (task=%q)", rec.Code, task)
		}
		var msg string
		select {
		case msg = <-runner.gotMessage:
		case <-time.After(2 * time.Second):
			t.Fatalf("dispatch never reached the orchestrator (task=%q)", task)
		}
		// gotMessage fires mid-dispatch, before it persists the snapshot/baseline
		// the NEXT call reads - wait for the whole dispatch to finish, not just this.
		waitInflightClear(t, ext, "github-acme-widgets-7")
		return msg
	}

	// 1. First review ever: full review (no baseline yet), and it DELIVERS -
	// the baseline should advance to just c1's patch-id.
	first := run(&fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed", deliverReview: true}, "review this", "WORK")
	if strings.Contains(first, "Focus your review on what's NEW") || strings.Contains(first, "already looked at every commit") {
		t.Errorf("first-ever review should carry no incremental scoping language:\n%s", first)
	}

	// 2. c2 lands on the PR.
	commitsJSON.Store(`[{"sha":"c1","commit":{"message":"add f1"}},{"sha":"c2","commit":{"message":"add f2"}}]`)

	// 3. A CONVERSATIONAL dispatch (no review delivered) - this advances the
	// GENERAL snapshot (comments/commits-as-seen) but must NOT touch the
	// review baseline.
	_ = run(&fakeRunner{gotMessage: make(chan string, 1), answer: "sure, here's my take", noPlan: true}, "what do you think so far? no need to re-review", "CONVERSATIONAL")

	// 4. A review request now MUST still see c2 as new - if the review scope
	// had been keyed off the general snapshot (the bug), c2 would already
	// read as "seen" because step 3 advanced it.
	second := run(&fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed", deliverReview: true}, "review this", "WORK")
	if !strings.Contains(second, "Focus your review on what's NEW") || !strings.Contains(second, "c2") {
		t.Errorf("review after a conversational dispatch must still scope to c2:\n%s", second)
	}
	if strings.Contains(second, "already looked at every commit") {
		t.Errorf("review under-scoped itself off the general snapshot instead of the review baseline:\n%s", second)
	}

	// 5. Step 4 DELIVERED a review covering c2 - the baseline now advances,
	// so the NEXT review sees zero new work.
	third := run(&fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed"}, "review this", "WORK")
	if !strings.Contains(third, "already looked at every commit") {
		t.Errorf("after the review in step 4 delivered, the next review should see zero new commits:\n%s", third)
	}
}

// TestLatestQuackVerdictReadsOwnPRReviewMarker pins #513's webhook half: an
// own-PR review submits as a real review (state COMMENTED, since GitHub
// disallows approve/request_changes on your own PR) carrying the actual
// verdict in the hidden marker - latestQuackVerdict must read that marker,
// not the state, or an own-PR approve would be misread as "comment".
func TestLatestQuackVerdictReadsOwnPRReviewMarker(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[{"state":"COMMENTED","body":"looks good\n\n<!-- quack:delivery:review:approve -->","user":{"login":"quack[bot]"},"submitted_at":"2026-07-20T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL
	app.installs["acme/widgets"] = 1
	app.tokens[1] = cachedToken{token: "ghs_x", expires: time.Now().Add(time.Hour)}
	ext := NewExtension(app, config.GitHubExtensionConfig{WebhookSecret: testSecret, Mention: "@quack"}, nil, nil, nil)

	verdict, err := ext.latestQuackVerdict(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("latestQuackVerdict: %v", err)
	}
	if verdict != "approve" {
		t.Errorf("verdict = %q; want %q (from the review body marker, not its COMMENTED state)", verdict, "approve")
	}
}

func TestVerifySignature(t *testing.T) {
	secret := []byte(testSecret)
	body := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name   string
		secret []byte
		body   []byte
		header string
		want   bool
	}{
		{"valid", secret, body, valid, true},
		{"missing header", secret, body, "", false},
		{"no prefix", secret, body, strings.TrimPrefix(valid, "sha256="), false},
		{"tampered body", secret, append(body, '!'), valid, false},
		{"wrong secret", []byte("other"), body, valid, false},
		{"empty secret", nil, body, valid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifySignature(tt.secret, tt.body, tt.header); got != tt.want {
				t.Errorf("verifySignature = %v; want %v", got, tt.want)
			}
		})
	}
}

// issuesBody builds an "issues" event payload for the label-driven workflow.
func issuesBody(action, labelName, sender string, isPR bool) []byte {
	pr := ""
	if isPR {
		pr = `"pull_request":{},`
	}
	return []byte(fmt.Sprintf(`{
		"action":%q,
		"issue":{"number":7,"title":"Add widget cache","body":"Widgets are refetched on every request.",%s"labels":[]},
		"label":{"name":%q},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},
		"sender":{"login":%q}
	}`, action, pr, labelName, sender))
}

// TestHandleWebhookBotCommentIgnored pins the bot-sender guard: quack's own
// posted comments (and any other bot's) must never trigger a run, or label
// workflows would chain into comment storms.
func TestHandleWebhookBotCommentIgnored(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "hi"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"mention"}, "")

	body := []byte(`{
		"action":"created",
		"comment":{"id":999,"body":"@quack review this","user":{"login":"quack[bot]"}},
		"issue":{"number":7},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`)
	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 no-op ack", rec.Code)
	}
	// The bot-suffix check is synchronous, before any goroutine is spawned.
	if atomic.LoadInt32(&runner.calls) != 0 {
		t.Error("a bot-authored mention must not dispatch a run")
	}
}

// TestHandleWebhookInvokerAllowlist pins issue #357: a mention only dispatches
// when its commenter is in github.allowed_users (case-insensitive), and an
// empty allowlist is a secure DENY-ALL default rather than allow-all.
func TestHandleWebhookInvokerAllowlist(t *testing.T) {
	tests := []struct {
		name         string
		allowedUsers []string
		invoker      string
		wantRun      bool
	}{
		{"allowed invoker dispatches", []string{"alice"}, "alice", true},
		{"allowed invoker matches case-insensitively", []string{"Alice"}, "alice", true},
		{"disallowed invoker does not dispatch", []string{"alice"}, "mallory", false},
		{"empty allowlist denies everyone", nil, "alice", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 1)
			gh := stubGitHub(t, posted)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "done"}
			keyPEM, _ := testKeyPEM(t)
			app, err := NewApp("1", keyPEM)
			if err != nil {
				t.Fatalf("NewApp: %v", err)
			}
			app.apiBase = gh.URL
			ext := NewExtension(app, config.GitHubExtensionConfig{
				WebhookSecret: testSecret,
				Mention:       "@quack",
				AllowedUsers:  tt.allowedUsers,
			}, runner, nil, nil)

			body := []byte(fmt.Sprintf(`{
				"action":"created",
				"comment":{"id":999,"body":"@quack add a feature","user":{"login":%q}},
				"issue":{"number":7},
				"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
				"installation":{"id":5}
			}`, tt.invoker))

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issue_comment", body))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if tt.wantRun {
				select {
				case <-runner.gotMessage:
				case <-time.After(2 * time.Second):
					t.Fatal("allowed invoker did not dispatch a run")
				}
			} else {
				// isInvokerAllowed is checked synchronously before any goroutine is spawned.
				if atomic.LoadInt32(&runner.calls) != 0 {
					t.Errorf("%s: must not dispatch a run", tt.name)
				}
			}
		})
	}
}

// TestHandleWebhookIssueLabelRespectsAllowlist pins the issues-labeled
// (quack:plan/quack:implement) enforcement point: a sender outside
// allowed_users never dispatches, even though the label itself required repo
// write access.
func TestHandleWebhookIssueLabelRespectsAllowlist(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "the plan"}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL
	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret,
		Mention:       "@quack",
		Triggers:      []string{"issue_plan"},
		AllowedUsers:  []string{"alice"},
	}, runner, nil, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "mallory", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// isInvokerAllowed is checked synchronously in handleIssues before any
	// goroutine is spawned.
	if atomic.LoadInt32(&runner.calls) != 0 {
		t.Error("issue-labeled sender not in allowed_users must not dispatch")
	}
}

// TestHandleWebhookMergeLabelRespectsAllowlist pins the merge-label
// enforcement point: a sender outside allowed_users can never authorize a
// merge, even with an APPROVED review already on the PR.
func TestHandleWebhookMergeLabelRespectsAllowlist(t *testing.T) {
	approved := `[{"state":"APPROVED","user":{"login":"quack[bot]"}}]`
	posted := make(chan string, 2)
	merged := make(chan struct{}, 1)
	gh := mergeStub(t, approved, "", posted, merged)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1)}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL
	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret,
		Mention:       "@quack",
		Triggers:      []string{"merge"},
		AllowedUsers:  []string{"alice"},
	}, runner, nil, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("mallory")))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// isInvokerAllowed is checked synchronously before any goroutine is spawned.
	select {
	case <-merged:
		t.Error("merge-label sender not in allowed_users must not authorize a merge")
	default:
	}
}

// TestHandleWebhookAutoReviewIgnoresAllowlist pins that the synthetic
// pr_opened auto-review has no human invoker and fires regardless of
// allowed_users (including empty/deny-all) - the allowlist gates
// human-invoked triggers only.
func TestHandleWebhookAutoReviewIgnoresAllowlist(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed"}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL
	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret,
		Mention:       "@quack",
		Triggers:      []string{"pr_opened"},
		// AllowedUsers intentionally left unset (deny-all for human triggers) -
		// must not block the synthetic auto-review.
	}, runner, nil, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-runner.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("pr_opened auto-review must fire regardless of allowed_users")
	}
}

func TestHandleWebhookIssuePlanLabel(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		label    string
		sender   string
		isPR     bool
		wantRun  bool
	}{
		{"plan label + issue_plan trigger fires", []string{"issue_plan"}, "quack:plan", "alice", false, true},
		{"non-matching label is a no-op", []string{"issue_plan"}, "bug", "alice", false, false},
		{"trigger not enabled is a no-op", []string{"mention"}, "quack:plan", "alice", false, false},
		{"bot sender is a no-op", []string{"issue_plan"}, "quack:plan", "quack[bot]", false, false},
		{"PR-shaped issue is a no-op", []string{"issue_plan"}, "quack:plan", "alice", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 1)
			gh := stubGitHub(t, posted)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), gotSessionID: make(chan string, 1), answer: "the plan"}
			ext := newTestExtensionWithTriggers(t, runner, gh.URL, tt.triggers, "")

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", tt.label, tt.sender, tt.isPR)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if !tt.wantRun {
				// The label/sender/trigger filters in handleIssues all decide
				// synchronously, before any goroutine is spawned.
				if atomic.LoadInt32(&runner.calls) != 0 {
					t.Error("issues event should not have dispatched a run")
				}
				return
			}
			select {
			case msg := <-runner.gotMessage:
				if !strings.Contains(msg, "Add widget cache") || !strings.Contains(msg, "implementation plan") {
					t.Errorf("plan message missing issue context: %q", msg)
				}
				if !strings.Contains(msg, "PLANNING-ONLY") {
					t.Errorf("plan message not framed planning-only: %q", msg)
				}
				// #569: the plan-only prompt must state that the answer text IS the
				// deliverable, not a pointer to a file the run wrote and discarded.
				if !strings.Contains(msg, "ANSWER TEXT is the plan") {
					t.Errorf("plan message does not state the answer text is the deliverable: %q", msg)
				}
				// #662: the file-path and stale-version cautions are constant, not
				// per-event - they moved to agents/orchestrator/prompt.md, so the
				// trigger itself no longer carries them (see
				// TestOrchestratorPromptCarriesMovedPlanOnlyCautions).
				for _, moved := range []string{"discarded", "current stable"} {
					if strings.Contains(msg, moved) {
						t.Errorf("plan message still carries the %q caution - it should have moved to agents/orchestrator/prompt.md: %q", moved, msg)
					}
				}
				// The vetting completion gate reads delivery demands off the task text:
				// a planning run must carry no push/PR instructions.
				for _, banned := range []string{"git_push", "github_pull_request", "create a branch"} {
					if strings.Contains(msg, banned) {
						t.Errorf("planning-only message contains delivery instruction %q: %q", banned, msg)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("plan label did not dispatch a run")
			}
			select {
			case sid := <-runner.gotSessionID:
				if sid != "github-acme-widgets-7" {
					t.Errorf("sessionID = %q, want issue-tied github-acme-widgets-7", sid)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no session id recorded")
			}
			// The plan is delivered as the fallback summary comment.
			select {
			case c := <-posted:
				if !strings.Contains(c, "the plan") {
					t.Errorf("posted comment = %q, want the run answer", c)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("plan was not posted back as an issue comment")
			}
		})
	}
}

// TestHandleWebhookIssueOpenedNoOp pins that non-labeled issue actions are
// ignored - the workflow is label-driven, not event-driven.
func TestHandleWebhookIssueOpenedNoOp(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1)}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"issue_plan"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("opened", "", "alice", false)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 no-op ack", rec.Code)
	}
	// The action=="labeled" filter is checked synchronously before any
	// goroutine is spawned.
	if atomic.LoadInt32(&runner.calls) != 0 {
		t.Error("issues opened should not dispatch a run")
	}
}

func TestHandleWebhookIssueImplementLabel(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		label    string
		wantRun  bool
	}{
		{"implement label + trigger fires", []string{"issue_implement"}, "quack:implement", true},
		{"trigger not enabled is a no-op", []string{"issue_plan"}, "quack:implement", false},
		{"plan label does not fire implement", []string{"issue_implement"}, "quack:plan", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 4)
			gh := stubGitHub(t, posted)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), gotSessionID: make(chan string, 1), answer: "done"}
			ext := newTestExtensionWithTriggers(t, runner, gh.URL, tt.triggers, "")

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", tt.label, "alice", false)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if !tt.wantRun {
				// The label/trigger filters in handleIssues decide synchronously,
				// before any goroutine is spawned.
				if atomic.LoadInt32(&runner.calls) != 0 {
					t.Error("issues event should not have dispatched a run")
				}
				return
			}
			// The implement task message is verified below. No canned ack comment
			// is posted - the orchestrator's initial response serves as the ack.
			select {
			case msg := <-runner.gotMessage:
				for _, want := range []string{"Closes #7", `<issue number="7">`} {
					if !strings.Contains(msg, want) {
						t.Errorf("implement message missing %q: %q", want, msg)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("implement label did not dispatch a run")
			}
			select {
			case sid := <-runner.gotSessionID:
				if sid != "github-acme-widgets-7" {
					t.Errorf("sessionID = %q, want the issue's session (plan continuity)", sid)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no session id recorded")
			}
		})
	}
}

// TestDispatchAttachesDeterministicGitHubSetup is #661: an issue-implement
// label run must carry repo/base_ref/work_branch off the webhook event
// itself, so the plan tool never has to ask the planner for them.
func TestDispatchAttachesDeterministicGitHubSetup(t *testing.T) {
	posted := make(chan string, 4)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), gotCtx: make(chan context.Context, 1), answer: "done"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"issue_implement"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var runCtx context.Context
	select {
	case runCtx = <-runner.gotCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("implement label did not dispatch a run")
	}
	setup, ok := tools.GitHubSetupFromContext(runCtx)
	if !ok {
		t.Fatal("no deterministic Setup attached to the run context")
	}
	if setup.Repo != "https://github.com/acme/widgets.git" {
		t.Errorf("Repo = %q, want the repository's clone_url", setup.Repo)
	}
	if setup.BaseRef != "main" {
		t.Errorf("BaseRef = %q, want the repository's default_branch", setup.BaseRef)
	}
	if setup.WorkBranch != "quack/issue-7" {
		t.Errorf("WorkBranch = %q, want quack/issue-7 (issue #7, no PR yet)", setup.WorkBranch)
	}
}

// TestDispatchResetsSessionForLabelWorkRequest pins T4 session hygiene: a
// LABEL-driven work request (quack:implement) resets the session before
// running, so a new attempt is not poisoned by a prior attempt's history -
// unlike a conversational @mention, which keeps full history for continuity
// (TestDispatchDoesNotResetSessionForMention, below).
func TestDispatchResetsSessionForLabelWorkRequest(t *testing.T) {
	posted := make(chan string, 4)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "done"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"issue_implement"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-runner.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("implement label did not dispatch a run")
	}
	select {
	case body := <-posted: // the run's fallback summary comment (no canned ack posted anymore)
		if !strings.Contains(body, "done") {
			t.Errorf("summary comment = %q; want the runner's answer", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no summary comment posted")
	}
	if got := atomic.LoadInt32(&runner.resets); got != 1 {
		t.Errorf("ResetSession called %d times for a label-driven work request; want 1", got)
	}
}

func TestDispatchDoesNotResetSessionForMention(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "done"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"mention"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack implement a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
	if got := atomic.LoadInt32(&runner.resets); got != 0 {
		t.Errorf("ResetSession called %d times for a conversational @mention; want 0 (needs continuity)", got)
	}
}

// TestFetchSnapshotRequiredMetaFailureSurfacesAsUnusable pins #467's first
// guard: when the required meta call (issueMeta) fails persistently (retries
// at the HTTP layer exhausted), loadGithubContext must flag the context as
// UNAVAILABLE - not silently return an empty-but-"valid" firstLoad snapshot,
// which is indistinguishable from a legitimately empty new issue.
func TestFetchSnapshotRequiredMetaFailureSurfacesAsUnusable(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case isIssueMetaPath(r.URL.Path):
			// Persistently unavailable - outlives doJSON's own retry budget.
			http.Error(w, `{"message":"No server is currently available to service your request."}`, http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	ext := newTestExtension(t, &fakeRunner{}, gh.URL)
	got := ext.loadGithubContext(context.Background(), "sess-1", "acme", "widgets", 7, false, 0, false)
	if !got.contextUnavailable {
		t.Error("contextUnavailable = false; want true when the required meta fetch fails")
	}
	if !got.firstLoad {
		t.Error("firstLoad = false; want true (no snapshot to diff against)")
	}
}

// TestDispatchAbortsLabelImplementWhenContextUnavailable pins #467's second
// guard: a label-triggered implement whose GitHub context could not be
// loaded (required fetch failed) must NOT dispatch "implement per the plan"
// to the runner - it must abort with an honest comment instead.
func TestDispatchAbortsLabelImplementWhenContextUnavailable(t *testing.T) {
	posted := make(chan string, 4)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case isIssueMetaPath(r.URL.Path):
			// The transient 503 from #467's diagnosis, persisting past the retry budget.
			http.Error(w, `{"message":"No server is currently available to service your request."}`, http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "done"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"issue_implement"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	// Since implement no longer posts a canned ack, only the abort comment lands.
	var abortComment string
	select {
	case abortComment = <-posted:
	case <-time.After(5 * time.Second):
		t.Fatal("no abort comment posted")
	}
	if !strings.Contains(abortComment, "not running blind") || !strings.Contains(abortComment, "Re-apply the label") {
		t.Errorf("abort comment = %q; want the don't-run-blind message", abortComment)
	}

	// The runner must never have been asked to implement anything.
	select {
	case msg := <-runner.gotMessage:
		t.Errorf("runner.Run was called with %q; want no dispatch when context is unavailable", msg)
	case <-time.After(200 * time.Millisecond):
	}
	if got := atomic.LoadInt32(&runner.calls); got != 0 {
		t.Errorf("runner.calls = %d; want 0 (must not implement blind)", got)
	}
}

// TestDispatchCollapsesPriorPlanComment pins plan half: when a NEW plan
// is posted for an issue, any PRIOR quack plan comment (carrying the plan
// delivery marker) is minimized via GraphQL before the new one lands, so the
// thread shows the current plan, not a pile of dead attempts.
func TestDispatchCollapsesPriorPlanComment(t *testing.T) {
	posted := make(chan string, 1)
	var minimizedID string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated) // the label-triggered 👀 ack (#252); irrelevant here
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[{"id":11,"node_id":"PLAN1","body":"## Old Plan\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			data, _ := io.ReadAll(r.Body)
			var b struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(data, &b)
			minimizedID = b.Variables.ID
			fmt.Fprint(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "## New Plan\n1. do the thing"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"issue_plan"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "New Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("posted plan comment = %q; want the new plan carrying its delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted")
	}
	if minimizedID != "PLAN1" {
		t.Errorf("minimizeComment subjectId = %q; want the prior plan comment's node_id PLAN1", minimizedID)
	}
}

// TestDispatchMarksCommentTriggeredPlan pins #731 test case 1: a plan
// requested via a /quack comment (not the quack:plan label) still carries
// the plan delivery marker on its tail comment.
func TestDispatchMarksCommentTriggeredPlan(t *testing.T) {
	posted := make(chan string, 1)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "## The Plan\n1. do the thing"}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("comment-triggered plan comment = %q; want it to carry the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted")
	}
}

// TestDispatchClassifiesIssueDeliverableOnce pins the single-call property a
// review of #731 caught: deliverableText (via buildEnvelope AND
// buildWorkerAsk) and deliverableIsPlan all need classifyIssueDeliverable's
// answer for the same run. Without memoization each calls the classifier
// independently, and a live model can disagree with itself between calls -
// the envelope telling the worker to produce a plan while the tail decides
// it wasn't one and skips the marker. quack:implement is granted so the
// classifier is actually consulted (never OpenPR-bounded away for free).
func TestDispatchClassifiesIssueDeliverableOnce(t *testing.T) {
	posted := make(chan string, 1)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open","labels":[{"name":"quack:implement"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	classifier := &fakeIntentClassifier{issueDeliverable: "COMMENT"}
	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "## The Plan\n1. do the thing"}
	ext := newTestExtension(t, runner, gh.URL)
	ext.SetIntentClassifier(classifier)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("plan comment = %q; want it to carry the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted")
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 1 {
		t.Errorf("issue deliverable classifier called %d times; want exactly 1 - buildEnvelope, buildWorkerAsk, and the plan-marker decision must share one answer", calls)
	}
}

// TestDispatchCollapsesPriorCommentTriggeredPlan pins #731 test case 2: two
// successive comment-triggered plan runs - the first is minimized before the
// second posts, exactly like the label-triggered case above.
func TestDispatchCollapsesPriorCommentTriggeredPlan(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[]`)
	posted := make(chan string, 2)
	var minimizedID atomic.Value
	minimizedID.Store("")

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON.Load().(string))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			data, _ := io.ReadAll(r.Body)
			var b struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(data, &b)
			minimizedID.Store(b.Variables.ID)
			fmt.Fprint(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "## First Plan\n1. step one"}
	ext := newTestExtension(t, runner, gh.URL)
	const sessionID = "github-acme-widgets-7"

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Fatalf("first plan comment = %q; want the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no first plan comment posted")
	}
	waitInflightClear(t, ext, sessionID)

	// The first plan is now on GitHub, marker and all - the second run's collapse must find it.
	commentsJSON.Store(`[{"id":11,"node_id":"PLAN1","body":"## First Plan\n1. step one\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)
	runner.answer = "## Second Plan\n1. step two"

	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack plan this again")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second dispatch status = %d; want 202", rec2.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "Second Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("second plan comment = %q; want the new plan carrying its delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no second plan comment posted")
	}
	if got := minimizedID.Load().(string); got != "PLAN1" {
		t.Errorf("minimizeComment subjectId = %q; want the first plan comment's node_id PLAN1", got)
	}
}

// TestDispatchCollapsesCommentTriggeredPlanOnLabelReplan pins #731 test case
// 3 (mixed triggers): a comment-triggered plan, then a label-triggered
// replan - the comment-triggered predecessor must still be minimized, which
// only works because the FIRST run also carried the marker.
func TestDispatchCollapsesCommentTriggeredPlanOnLabelReplan(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[]`)
	posted := make(chan string, 2)
	var minimizedID atomic.Value
	minimizedID.Store("")

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON.Load().(string))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			data, _ := io.ReadAll(r.Body)
			var b struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(data, &b)
			minimizedID.Store(b.Variables.ID)
			fmt.Fprint(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "## Comment-Triggered Plan\n1. step one"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"mention", "issue_plan"}, "")
	const sessionID = "github-acme-widgets-7"

	// First: a plain /quack comment asks for a plan.
	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("comment-triggered dispatch status = %d; want 202", rec1.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Fatalf("comment-triggered plan comment = %q; want the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment-triggered plan comment posted")
	}
	waitInflightClear(t, ext, sessionID)

	commentsJSON.Store(`[{"id":11,"node_id":"PLAN1","body":"## Comment-Triggered Plan\n1. step one\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)
	runner.answer = "## Labeled Plan\n1. step two"

	// Second: a maintainer applies quack:plan to re-plan properly.
	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("label-triggered dispatch status = %d; want 202", rec2.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "Labeled Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("label-triggered plan comment = %q; want the new plan carrying its delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no label-triggered plan comment posted")
	}
	if got := minimizedID.Load().(string); got != "PLAN1" {
		t.Errorf("minimizeComment subjectId = %q; want the comment-triggered plan's node_id PLAN1 - the label-triggered replan must collapse it", got)
	}
}

// TestDispatchImplementRunUntouchedByPlanCollapse pins #731 test case 4: a
// non-plan deliverable's tail comment carries no plan marker and triggers no
// collapse, even when nothing was "delivered" (fakeRunner stages no PR), so
// the run still falls through to the same tail-comment path a plan does.
func TestDispatchImplementRunUntouchedByPlanCollapse(t *testing.T) {
	posted := make(chan string, 1)
	var graphqlCalled int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			atomic.AddInt32(&graphqlCalled, 1)
			fmt.Fprint(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Add widget cache","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "implemented the change"}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"issue_implement"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case body := <-posted:
		if strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("implement run's tail comment = %q; must not carry the plan marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no tail comment posted")
	}
	if got := atomic.LoadInt32(&graphqlCalled); got != 0 {
		t.Errorf("graphql (minimizeComment) called %d times; want 0 - an implement run must never trigger plan collapse", got)
	}
}

// TestDispatchPostsPlanWhenCollapseFails pins #731 test case 5: collapse
// stays best-effort - a GraphQL minimizeComment failure must not block or
// fail the new plan's delivery.
func TestDispatchPostsPlanWhenCollapseFails(t *testing.T) {
	posted := make(chan string, 1)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[{"id":11,"node_id":"PLAN1","body":"## Old Plan\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			http.Error(w, `{"errors":[{"message":"internal error"}]}`, http.StatusInternalServerError)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Some issue","body":"","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "## New Plan\n1. do the thing"}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "New Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("plan comment = %q; want it posted with its marker despite the collapse failure", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted - a collapse failure must not block delivery")
	}
}

// TestPlanTaskNoIssueBodyDuplicate pins #619 defect 2: planTask must not
// embed the issue body - runMessage's #459 context block (gh.text) already
// carries it verbatim, so planTask's own copy is a straight duplicate of the
// same text in the same prompt.
func TestPlanTaskNoIssueBodyDuplicate(t *testing.T) {
	var p issuesPayload
	p.Issue.Number = 7
	p.Issue.Title = "Add widget cache"
	p.Issue.Body = "Widgets are refetched on every request."
	msg := planTask(p)
	if !strings.Contains(msg, "Add widget cache") {
		t.Errorf("planTask missing the issue title:\n%s", msg)
	}
	if strings.Contains(msg, p.Issue.Body) {
		t.Errorf("planTask embeds the issue body itself - runMessage's context block already carries it:\n%s", msg)
	}
}

// TestImplementTaskCore pins implementTask's own contribution - the issue
// number/title and the delivery instructions. The discussion (the approved
// plan) is no longer implementTask's job: it arrives via dispatch's unified
// loadGithubContext, the same path every other trigger uses (#459) - see
// TestHandleWebhookIssueImplementLabel for that end-to-end.
func TestImplementTaskCore(t *testing.T) {
	var p issuesPayload
	p.Issue.Number = 7
	p.Issue.Title = "Add widget cache"
	p.Issue.Body = "Widgets are refetched on every request."
	msg := implementTask(p, nil, "quack:partial-fix")
	for _, want := range []string{"Implement issue #7", "Add widget cache", "Closes #7", "stage_pr", "Never merge"} {
		if !strings.Contains(msg, want) {
			t.Errorf("implementTask missing %q:\n%s", want, msg)
		}
	}

	// A CUSTOM configured partial-fix label is what's honoured - not a hardcoded
	// default (the blocking finding on #505).
	custom := implementTask(p, []string{"bug", "my-org:incomplete"}, "my-org:incomplete")
	if strings.Contains(custom, "`Closes #7`") {
		t.Errorf("custom partial-fix label ignored - Closes still present:\n%s", custom)
	}
	// The default string must NOT trigger partial-fix when a custom label is configured.
	notCustom := implementTask(p, []string{"quack:partial-fix"}, "my-org:incomplete")
	if !strings.Contains(notCustom, "`Closes #7`") {
		t.Errorf("non-matching label wrongly suppressed Closes:\n%s", notCustom)
	}

	// Partial-fix: should NOT instruct a Closes keyword.
	partialMsg := implementTask(p, []string{"bug", "quack:partial-fix"}, "quack:partial-fix")
	for _, absent := range []string{"`Closes #7`"} {
		if strings.Contains(partialMsg, absent) {
			t.Errorf("partial-fix task must not instruct closing with the keyword %q:\n%s", absent, partialMsg)
		}
	}
	for _, want := range []string{"part" + "ial fix", "Do NOT use a Closes keyword", "stage_pr"} {
		if !strings.Contains(partialMsg, want) {
			t.Errorf("partial-fix task missing %q:\n%s", want, partialMsg)
		}
	}
}

// mergeStub is stubGitHub plus a reviews list, an issue-comments list (own-PR
// verdict markers), and a merge endpoint; merged signals when the merge PUT
// lands. commentsJSON == "" defaults to an empty list.
// mergeStub serves both the merge-label handler's own REST surface
// (reviews/comments for latestQuackVerdict, PUT .../merge) and the rest of
// stubGitHub's dispatch surface (pulls meta, files, commits, reactions,
// issue meta) - the merge label can itself dispatch a review run (see
// mergeIfApproved), so the stub needs to serve that too.
func mergeStub(t *testing.T, reviewsJSON, commentsJSON string, posted chan<- string, merged chan<- struct{}) *httptest.Server {
	t.Helper()
	if commentsJSON == "" {
		commentsJSON = "[]"
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, reviewsJSON)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			merged <- struct{}{}
			fmt.Fprint(w, `{"merged":true}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, commentsJSON)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"A test issue.","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

// mergeStubDynamic is mergeStub with reviews served from a mutable value (an
// empty review list to start) so a test can simulate a review landing
// mid-dispatch via the returned setReviews - used only by
// TestHandleWebhookMergeLabelReviewLandsConsumesIntent. merged carries the
// PUT .../merge request body so a test can assert the head-sha merge guard.
func mergeStubDynamic(t *testing.T, posted chan<- string, merged chan<- string) (srv *httptest.Server, setReviews func(string)) {
	t.Helper()
	var reviews atomic.Value
	reviews.Store("[]")
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, reviews.Load().(string))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			body, _ := io.ReadAll(r.Body)
			merged <- string(body)
			fmt.Fprint(w, `{"merged":true}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"A test issue.","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	return srv, func(j string) { reviews.Store(j) }
}

func mergeLabelBody(sender string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"labeled",
		"number":7,
		"label":{"name":"quack:merge"},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},
		"sender":{"login":%q}
	}`, sender))
}

// TestHandleWebhookMergeLabel covers the cases where the merge label's fate is
// decided WITHOUT needing to dispatch a run: an approving review already
// exists (merges immediately, unchanged), a non-approving verdict already
// exists (refuses and leaves the standing intent recorded so a later approval
// - after fixes - can still merge it), the trigger is off, or the sender is a
// bot. The "no review at all yet" case dispatches a review run and is covered
// separately (TestHandleWebhookMergeLabelQueuesAndDispatchesReview) since it
// needs the runner wired up to observe the dispatch.
func TestHandleWebhookMergeLabel(t *testing.T) {
	approved := `[{"state":"CHANGES_REQUESTED","user":{"login":"quack[bot]"}},{"state":"APPROVED","user":{"login":"quack[bot]"}}]`
	tests := []struct {
		name        string
		triggers    []string
		reviews     string
		comments    string // own-PR verdict-marker comments; "" = none
		sender      string
		wantMerge   bool
		wantComment string // substring of the posted comment; "" = no comment expected
		wantIntent  bool   // whether a standing merge intent should be recorded
	}{
		{"approved review merges", []string{"merge"}, approved, "", "alice", true, "Merged", false},
		{"changes-requested stands by with the intent recorded", []string{"merge"},
			`[{"state":"APPROVED","user":{"login":"quack[bot]"}},{"state":"CHANGES_REQUESTED","user":{"login":"quack[bot]"}}]`,
			"", "alice", false, "Standing by: my latest review is request_changes, not an approval", true},
		{"COMMENTED carries no verdict but still stands by without a later approve", []string{"merge"},
			`[{"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"},{"state":"COMMENTED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-02T00:00:00Z"}]`,
			"", "alice", false, "Standing by: my latest review is comment, not an approval", true},
		{"own-PR comment-review marker approves and merges", []string{"merge"}, `[]`,
			`[{"user":{"login":"quack[bot]"},"body":"LGTM\n\n<!-- quack:delivery:review:approve -->","created_at":"2026-01-01T00:00:00Z"}]`,
			"alice", true, "Merged", false},
		{"own-PR comment-review marker request_changes stands by", []string{"merge"}, `[]`,
			`[{"user":{"login":"quack[bot]"},"body":"needs work\n\n<!-- quack:delivery:review:request_changes -->","created_at":"2026-01-01T00:00:00Z"}]`,
			"alice", false, "Standing by: my latest review is request_changes, not an approval", true},
		{"trigger not enabled is a no-op", []string{"mention"}, approved, "", "alice", false, "", false},
		{"bot sender cannot authorize", []string{"merge"}, approved, "", "other[bot]", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 2)
			merged := make(chan struct{}, 1)
			gh := mergeStub(t, tt.reviews, tt.comments, posted, merged)
			defer gh.Close()

			st := newFixTestStore(t)
			runner := &fakeRunner{gotMessage: make(chan string, 1)}
			ext := newFixExtension(t, runner, gh.URL, st, tt.triggers...)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody(tt.sender)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantComment != "" {
				select {
				case c := <-posted:
					if !strings.Contains(c, tt.wantComment) {
						t.Errorf("comment = %q, want substring %q", c, tt.wantComment)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("expected an outcome comment")
				}
			}
			if tt.wantMerge {
				select {
				case <-merged:
				case <-time.After(2 * time.Second):
					t.Fatal("expected a merge PUT")
				}
			} else {
				// mergePR always runs before comment() - the wait above (or, with no
				// comment, the synchronous gate in handlePullRequest) already settles this.
				select {
				case <-merged:
					t.Error("merge must not have been called")
				default:
				}
			}
			// None of these cases dispatches an orchestrator run - either the
			// verdict was already decided, or the trigger/sender gate refused first.
			if atomic.LoadInt32(&runner.calls) != 0 {
				t.Error("this case must not dispatch an orchestrator run")
			}

			intent, err := st.GetGithubMergeIntent(context.Background(), "github-acme-widgets-7")
			if err != nil {
				t.Fatalf("GetGithubMergeIntent: %v", err)
			}
			if tt.wantIntent && (intent == nil || intent.RequestedBy != "alice") {
				t.Errorf("intent = %+v; want a recorded standing intent for alice", intent)
			}
			if !tt.wantIntent && intent != nil {
				t.Errorf("intent = %+v; want none recorded", intent)
			}
		})
	}
}

// TestHandleWebhookMergeLabelQueuesAndDispatchesReview covers applying
// quack:merge to a PR quack has never looked at: the label becomes a standing
// intent AND dispatches a review itself - otherwise the label would silently
// do nothing until someone separately asked for a review.
func TestHandleWebhookMergeLabelQueuesAndDispatchesReview(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	gh := mergeStub(t, "[]", "", posted, merged)
	defer gh.Close()

	st := newFixTestStore(t)
	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed"}
	ext := newFixExtension(t, runner, gh.URL, st, "merge")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	select {
	case c := <-posted:
		if !strings.Contains(c, "Queued") || !strings.Contains(c, "Reviewing it now") {
			t.Errorf("comment = %q; want a queued+reviewing message", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no queued comment posted")
	}
	select {
	case msg := <-runner.gotMessage:
		if !strings.Contains(msg, "<deliverable>a review with inline comments and a verdict</deliverable>") {
			t.Errorf("dispatched envelope = %q; want the auto-review deliverable", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no review was auto-dispatched for the unreviewed PR")
	}

	intent, err := st.GetGithubMergeIntent(context.Background(), "github-acme-widgets-7")
	if err != nil || intent == nil || intent.RequestedBy != "alice" {
		t.Fatalf("GetGithubMergeIntent = %+v, %v; want a recorded intent for alice", intent, err)
	}
}

// TestHandleWebhookMergeLabelWaitsForInFlightReview covers applying
// quack:merge while a review is ALREADY running on the PR (a common race: the
// label lands while a review dispatched moments earlier is still in
// progress) - it must record the intent and wait, never dispatch a SECOND
// concurrent review on the same session.
func TestHandleWebhookMergeLabelWaitsForInFlightReview(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	gh := mergeStub(t, "[]", "", posted, merged)
	defer gh.Close()

	st := newFixTestStore(t)
	runner := &fakeRunner{gotMessage: make(chan string, 1), block: make(chan struct{}), answer: "reviewed"}
	ext := newFixExtension(t, runner, gh.URL, st, "mention", "merge")

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", pullCommentBody("@quack review this")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec1.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&runner.calls) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&runner.calls) != 1 {
		t.Fatal("the mention-triggered review never started")
	}

	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec2.Code)
	}

	select {
	case c := <-posted:
		if !strings.Contains(c, "already in progress") {
			t.Errorf("comment = %q; want it to note a review is already running", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no queued comment posted")
	}

	intent, err := st.GetGithubMergeIntent(context.Background(), "github-acme-widgets-7")
	if err != nil || intent == nil {
		t.Fatalf("GetGithubMergeIntent = %+v, %v; want a recorded intent", intent, err)
	}
	close(runner.block) // release the blocked run
	// Let it fully finish before the test ends, or it races defer gh.Close().
	waitInflightClear(t, ext, "github-acme-widgets-7")
	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Errorf("runner.calls = %d; want 1 (the label must not dispatch a second review while one is in flight)", got)
	}
}

// TestHandleWebhookMergeLabelReviewLandsConsumesIntent covers the standing
// intent's whole point: no review existed when quack:merge was applied, the
// label queued a review AND recorded the intent, and once that review is
// actually POSTED to GitHub with an approving verdict, the PR merges on its
// own - naming the original label-applier, not whoever (if anyone) is around
// when the review lands.
func TestHandleWebhookMergeLabelReviewLandsConsumesIntent(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan string, 1)
	gh, setReviews := mergeStubDynamic(t, posted, merged)
	defer gh.Close()

	st := newFixTestStore(t)
	runner := &fakeRunner{gotMessage: make(chan string, 1), block: make(chan struct{}), deliverReview: true}
	ext := newFixExtension(t, runner, gh.URL, st, "merge")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	select {
	case c := <-posted:
		if !strings.Contains(c, "Queued") {
			t.Errorf("comment = %q; want the queued message", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no queued comment posted")
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&runner.calls) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&runner.calls) != 1 {
		t.Fatal("the auto-dispatched review never started")
	}

	// The review "lands" as an approval right before the blocked run finishes
	// and records its delivery - simulating quack's own review being posted.
	setReviews(`[{"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"}]`)
	close(runner.block)

	select {
	case body := <-merged:
		if !strings.Contains(body, `"sha":"headsha1"`) {
			t.Errorf("merge request body = %q; want it pinned to the reviewed head sha", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a merge PUT once the review landed approving")
	}
	select {
	case c := <-posted:
		if !strings.Contains(c, "Merged") || !strings.Contains(c, "@alice") {
			t.Errorf("comment = %q; want it to name the original authorizer", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no merge comment posted")
	}

	intent, err := st.GetGithubMergeIntent(context.Background(), "github-acme-widgets-7")
	if err != nil || intent != nil {
		t.Errorf("intent = %+v, %v; want it cleared after the merge", intent, err)
	}
}

// TestHandleWebhookMergeLabelRestartSurvival pins that the standing intent
// (like GithubFixState) survives a process restart: a FRESH Extension over
// the SAME store - with no in-memory memory of the label event that recorded
// it - still honours it once a review lands.
func TestHandleWebhookMergeLabelRestartSurvival(t *testing.T) {
	st := newFixTestStore(t)
	sessionID := "github-acme-widgets-7"
	if err := st.SetGithubMergeIntent(context.Background(), sessionID, "alice"); err != nil {
		t.Fatalf("seed merge intent: %v", err)
	}

	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	approved := `[{"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"}]`
	gh := mergeStub(t, approved, "", posted, merged)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed", deliverReview: true}
	ext := newFixExtension(t, runner, gh.URL, st, "mention")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack review this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	select {
	case <-merged:
	case <-time.After(2 * time.Second):
		t.Fatal("the pre-restart standing intent did not merge once the review landed")
	}
	select {
	case c := <-posted:
		if !strings.Contains(c, "Merged") || !strings.Contains(c, "@alice") {
			t.Errorf("comment = %q; want it to name the pre-restart authorizer", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no merge comment posted")
	}

	intent, err := st.GetGithubMergeIntent(context.Background(), sessionID)
	if err != nil || intent != nil {
		t.Errorf("intent = %+v, %v; want it cleared after the merge", intent, err)
	}
}

// TestHandleWebhookPlanLabelPostsPlanEvenWhenDelivered pins the regression where a
// plan-only run silently dropped its plan: a label trigger implies work, so
// judgePassed made `delivered` true and dispatch skipped the summary comment -
// but a plan-only run's deliverable IS that comment.
// (Latent until github_comment was removed; before that the worker posted the plan
// itself, masking the skip.) The prior plan-label test never set judgePassed, so it
// couldn't catch this.
func TestHandleWebhookPlanLabelPostsPlanEvenWhenDelivered(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	// judgePassed:true + the work-verby stub issue ("Add widget cache") is exactly
	// the production condition: pre-fix, `delivered` was true and the plan was dropped.
	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "## Plan\n\nthe plan", judgePassed: true}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"issue_plan"}, "")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "the plan") {
			t.Errorf("posted comment is not the plan: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("plan-only run did not post its plan - the delivered-skip dropped it")
	}
}

// TestHandleWebhookLabelPostsEyesReactionOnIssue pins #252: a label-triggered run
// (quack:plan / quack:implement) posts an instant 👀 on the ISSUE - POST to
// /issues/{number}/reactions, NOT the comment-reaction endpoint (a label event
// carries no comment ID, so ackReaction can't be reused).
func TestHandleWebhookLabelPostsEyesReactionOnIssue(t *testing.T) {
	for _, tc := range []struct{ trigger, label string }{
		{"issue_plan", "quack:plan"},
		{"issue_implement", "quack:implement"},
	} {
		t.Run(tc.trigger, func(t *testing.T) {
			reacted := make(chan string, 1) // "<path> <body>" of the reaction POST
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/installation"):
					fmt.Fprint(w, `{"id":5}`)
				case strings.HasSuffix(r.URL.Path, "/access_tokens"):
					fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
				case strings.HasSuffix(r.URL.Path, "/reactions"):
					b, _ := io.ReadAll(r.Body)
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"id":1}`)
					select {
					case reacted <- r.URL.Path + " " + string(b):
					default:
					}
				default:
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{}`)
				}
			}))
			defer srv.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "ok"}
			ext := newTestExtensionWithTriggers(t, runner, srv.URL, []string{tc.trigger}, "")

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", tc.label, "alice", false)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			select {
			case got := <-reacted:
				if !strings.Contains(got, "/repos/acme/widgets/issues/7/reactions") {
					t.Errorf("reaction hit wrong endpoint: %q (want /issues/7/reactions)", got)
				}
				if !strings.Contains(got, `"content":"eyes"`) {
					t.Errorf("reaction content not eyes: %q", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no 👀 reaction posted on the issue for a label-triggered run")
			}
		})
	}
}

// mentionCommentBody is issueCommentBody with the issue's title present, as a
// real issue_comment payload carries it - used to pin #380's title backfill.
func mentionCommentBody(commentBody, issueTitle string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7,"title":%q},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody, issueTitle))
}

// TestDispatchGeneratesTitle pins #380: a GitHub-webhook-dispatched chat gets a
// real, non-placeholder title derived from the triggering issue - dispatch
// never called generateTitle/UpdateTitle before this fix, so the chat's Title
// column stayed empty forever (rendering as "New chat" in the UI).
func TestDispatchGeneratesTitle(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed"}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL
	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret,
		Mention:       "@quack",
		AllowedUsers:  []string{"alice"},
	}, runner, st, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", mentionCommentBody("@quack review this", "Widgets leak memory")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back; dispatch never completed")
	}

	sessionID := "github-acme-widgets-7"
	c, err := st.GetChat(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if c == nil {
		t.Fatal("chat row was never created")
	}
	if c.Title == "" || c.Title == "New chat" {
		t.Errorf("Title = %q; want a real title derived from the issue", c.Title)
	}
	if c.Title != "Widgets leak memory" {
		t.Errorf("Title = %q; want the issue title", c.Title)
	}
}

// TestDispatchTitleFromLabelDrivenIssue pins #380 for the label-driven path
// (quack:plan/quack:implement), which synthesizes its issueCommentPayload from
// an issuesPayload rather than a real webhook comment.
func TestDispatchTitleFromLabelDrivenIssue(t *testing.T) {
	reacted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
			select {
			case reacted <- struct{}{}:
			default:
			}
		default:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "## Plan\n\nthe plan", judgePassed: true}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL
	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret,
		Mention:       "@quack",
		Triggers:      []string{"issue_plan"},
		AllowedUsers:  []string{"alice"},
	}, runner, st, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	select {
	case <-reacted:
	case <-time.After(2 * time.Second):
		t.Fatal("label-triggered run never dispatched")
	}

	sessionID := "github-acme-widgets-7"
	deadline := time.Now().Add(2 * time.Second)
	var c *store.Chat
	for {
		c, err = st.GetChat(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("GetChat: %v", err)
		}
		if c != nil && c.Title != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat title never set for label-driven dispatch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.Title != "Add widget cache" {
		t.Errorf("Title = %q; want the issue title from issuesBody", c.Title)
	}
}

// TestDispatchDoesNotOverwriteExistingTitle pins the once-only semantics: a
// conversational follow-up on an already-titled session must not clobber the
// title a prior dispatch set - mirrors runChat's own titleCh guard.
func TestDispatchDoesNotOverwriteExistingTitle(t *testing.T) {
	posted := make(chan string, 2)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sessionID := "github-acme-widgets-7"
	if err := st.SetChatGitHub(context.Background(), sessionID, "acme/widgets", "https://github.com/acme/widgets/pull/7", "", "alice"); err != nil {
		t.Fatalf("SetChatGitHub: %v", err)
	}
	if err := st.UpdateTitle(context.Background(), sessionID, "Existing title"); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "reviewed"}
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL
	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret,
		Mention:       "@quack",
		AllowedUsers:  []string{"alice"},
	}, runner, st, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", mentionCommentBody("@quack what did you mean?", "A totally different title")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back; dispatch never completed")
	}

	c, err := st.GetChat(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if c.Title != "Existing title" {
		t.Errorf("Title = %q; want the pre-existing title preserved", c.Title)
	}
}

// TestDispatchPostsHITLCommentOnPause verifies that when drive() encounters a
// node_needs_input event (simulating an ask_user call), dispatch posts the
// HITL question as a GitHub comment instead of falling through to the "produced
// no answer" tail. The reply webhook resumes the same session via run →
// orchestrator.Run → resumeNodeRun.
func TestDispatchPostsHITLCommentOnPause(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "should not appear", hitInput: true}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack research and advise")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case body := <-posted:
		if !strings.Contains(body, "quack has a question before proceeding") {
			t.Errorf("posted comment missing HITL framing: %s", body)
		}
		if strings.Contains(body, "node-1") && !strings.Contains(body, "version of Go") {
			t.Errorf("HITL comment should carry node name and question: %s", body)
		}
		if strings.Contains(body, "produced no answer") {
			t.Errorf("HITL pause should NOT post the 'produced no answer' tail; got: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no HITL comment posted after ask_user pause")
	}
}

// TestDispatchSkipsNudgeOnPause verifies that dispatch does NOT nudge a run that
// hit a HITL pause - the nudge is only for runs that produced no plan but were
// otherwise complete (not paused).
func TestDispatchSkipsNudgeOnPause(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 4), answer: "ok", hitInput: true}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Errorf("runner invoked %d times, want 1 (HITL pause must not trigger the nudge)", got)
	}
}

// dedupRunner is a fakeRunner that also records which Run() calls it receives
// via channel `runCalls`, enabling dispatch tests to detect whether a second
// call was dropped by the in-flight guard.
type dedupRunner struct {
	*fakeRunner
	runCalls chan string // sessionIDs of every Run() call (blocks until read)
}

func newDedupRunner() *dedupRunner {
	return &dedupRunner{
		fakeRunner: &fakeRunner{gotMessage: make(chan string, 4), answer: "ok"},
		runCalls:   make(chan string, 4),
	}
}

func (d *dedupRunner) Run(ctx context.Context, label string, sessionID string, message string, parts []*genai.Part) iter.Seq2[stream.SSEEvent, error] {
	select {
	case d.runCalls <- sessionID:
	default:
	}
	return d.fakeRunner.Run(ctx, label, sessionID, message, parts)
}

// TestDispatchDedupNearSimultaneousVerifiesTheInflightGuard checks that when two
// webhooks hit the same Extension within milliseconds (same issue → same sessionID),
// only ONE dispatch actually runs - LoadOrStore claims it and the second returns
// early. Also verifies that after the first run completes, a new dispatch succeeds.
func TestDispatchDedupNearSimultaneousVerifiesTheInflightGuard(t *testing.T) {
	dr := newDedupRunner()
	posted := make(chan string, 2)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL

	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret, Mention: "@quack", AllowedUsers: []string{"alice"},
	}, dr, nil, nil)

	dr.fakeRunner.block = make(chan struct{}) // block first dispatch so inflight stays

	// Fire two mentions on the SAME issue (#7). Both ack 202 and spawn a goroutine.
	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack first")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}

	// The handleWebhook goroutine for the first mention will call dispatch,
	// which claims sessionID in inflight, then hits dr.block and stays there.
	// Fire a second mention immediately - dispatch should find sessionID in
	// inflight, ack with a best-effort 👀 reaction, and return early.
	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack second")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second dispatch status = %d; want 202 (handler acks even if deduped)", rec2.Code)
	}

	// The winner pushes into runCalls as soon as Run() is entered, before it
	// ever reaches dr.block - drain that expected first entry.
	firstSession := ""
	select {
	case firstSession = <-dr.runCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("no Run() call recorded")
	}
	if firstSession != "github-acme-widgets-7" {
		t.Errorf("expected sessionID %q, got %q", "github-acme-widgets-7", firstSession)
	}

	// A wrongly-undeduped second dispatch would also reach Run() well before
	// it could ever hit dr.block - catch it here if it happens.
	select {
	case sid := <-dr.runCalls:
		t.Fatalf("second concurrent trigger on same sessionID should have been deduplicated by LoadOrStore: sid=%q", sid)
	case <-time.After(200 * time.Millisecond):
	}

	// Now let the first dispatch complete. After it finishes, its defer should
	// delete the inflight entry, allowing a third dispatch to proceed.
	close(dr.fakeRunner.block)

	// Let the first dispatch finish and observe it posting. Then verify that a
	// third dispatch (after the run completes) succeeds - inflight entry was cleaned up.
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never posted its answer")
	}

	// The inflight claim clears in dispatch's deferred Delete, which runs on
	// RETURN - after the post above. Wait for the actual release before the third
	// trigger, else it races the release and gets wrongly deduped under load.
	waitInflightClear(t, ext, "github-acme-widgets-7")

	rec3 := httptest.NewRecorder()
	ext.handleWebhook(rec3, signedRequest("issue_comment", issueCommentBody("@quack third")))
	if rec3.Code != http.StatusAccepted {
		t.Fatalf("third dispatch status = %d; want 202", rec3.Code)
	}

	select {
	case sid := <-dr.runCalls:
		if sid != "github-acme-widgets-7" {
			t.Errorf("expected sessionID github-acme-widgets-7 after reuse, got %q", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third dispatch didn't run - inflight entry wasn't cleaned up after the first completed")
	}

	if got := atomic.LoadInt32(&dr.calls); got != 2 {
		t.Errorf("runner calls = %d, want 2 (first + third; second was deduplicated)", got)
	}
	// Let the third dispatch finish before defer gh.Close() runs, or it races it.
	waitInflightClear(t, ext, "github-acme-widgets-7")
}

// TestDispatchDedupDifferentSessionsAllowsConcurrent verifies that dispatches on
// DIFFERENT sessions (different issues/PRs) all proceed - the inflight guard only
// blocks duplicate sessionIDs.
func TestDispatchDedupDifferentSessionsAllowsConcurrent(t *testing.T) {
	dr := newDedupRunner()
	posting := make(chan string, 2)
	gh := stubGitHub(t, posting)
	defer gh.Close()

	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = gh.URL

	ext := NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret, Mention: "@quack", AllowedUsers: []string{"alice"},
	}, dr, nil, nil)

	for _, issueNum := range []int{7, 8} {
		body := fmt.Sprintf(`{
			"action":"created",
			"comment":{"id":999,"body":"@quack task %d","user":{"login":"alice"}},
			"issue":{"number":%d},
			"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
			"installation":{"id":5}
		}`, issueNum, issueNum)
		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("issue_comment", []byte(body)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("issue #%d dispatch status = %d; want 202", issueNum, rec.Code)
		}
	}

	received := make(map[string]bool)
	for i := 0; i < 2; i++ {
		select {
		case sid := <-dr.runCalls:
			received[sid] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("expected %d dispatch run calls, only got %d", 2-len(received), len(received))
		}
	}

	if !received["github-acme-widgets-7"] {
		t.Error("missing sessionID for issue #7")
	}
	if !received["github-acme-widgets-8"] {
		t.Error("missing sessionID for issue #8")
	}
}

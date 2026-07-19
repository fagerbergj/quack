package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

const testSecret = "shhh-webhook-secret"

// fakeRunner records the dispatched message and returns a fixed answer. Run's
// iterator optionally blocks on `block` (to prove the handler acks before the
// run finishes).
type fakeRunner struct {
	gotMessage   chan string
	gotSessionID chan string
	answer       string
	block        chan struct{}
	calls        int32
	noPlan       bool // when true, emit no dag_plan event (simulates a narration-only turn)
	// judgePassed simulates a node's trust gate clearing its judge round (a
	// node_done event with JudgePassed) — dispatch's fallback proxy for
	// "delivery was attempted" when nothing recorded an authoritative outcome
	// (see takeDeliveryResult).
	judgePassed bool
	// deliverOK/deliverErr simulate the trust gate's own commitDelivery
	// having ALREADY run and recorded its outcome (recordDeliveryResult) — the
	// authoritative signal dispatch prefers over judgePassed. deliverOK records
	// a success; deliverErr (mutually exclusive) records a failure.
	deliverOK  bool
	deliverErr string
	// resets counts ResetSession calls — dispatch's T4 session-hygiene signal.
	resets int32
}

func (f *fakeRunner) ResetSession(context.Context, string, string) error {
	atomic.AddInt32(&f.resets, 1)
	return nil
}

func (f *fakeRunner) Run(_ context.Context, _, sessionID, message string, _ []*genai.Part) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		atomic.AddInt32(&f.calls, 1)
		if f.block != nil {
			<-f.block
		}
		if !f.noPlan {
			yield(stream.SSEEvent{Name: stream.EventDagPlan}, nil) // a real run executes a plan
		}
		if f.judgePassed {
			yield(stream.SSEEvent{Name: stream.EventNodeDone, Data: stream.NodeDoneData{JudgePassed: true}}, nil)
		}
		if f.deliverOK {
			recordDeliveryResult(sessionID, nil)
		} else if f.deliverErr != "" {
			recordDeliveryResult(sessionID, fmt.Errorf("%s", f.deliverErr))
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

// planRunner is fakeRunner's dag_plan event with real DagPlanData (PlanID +
// one node), needed to assert the plan actually gets mirrored into the store —
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
			fmt.Fprint(w, `[]`) // changed-files list (gatherReviewContext); overridden per-test where it matters
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`) // no prior review by default: first-time framing
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			postedComment <- string(body)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`) // GET: review-comment / conversation list (gatherReviewContext)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
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
		"installation":{"id":5}
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
					if !strings.Contains(msg, "acme/widgets") || !strings.Contains(msg, "pull_number=7") {
						t.Errorf("run message missing repo/pull_number: %q", msg)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("pr_opened trigger did not dispatch a run")
				}
			} else {
				time.Sleep(50 * time.Millisecond)
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
				time.Sleep(50 * time.Millisecond)
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
				time.Sleep(50 * time.Millisecond)
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

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "done — opened PR #12"}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case msg := <-runner.gotMessage:
		if !strings.Contains(msg, "add a feature") || !strings.Contains(msg, "acme/widgets") {
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

// A webhook-dispatched run must persist a turn + DAG-carrying chat_events for
// its session, exactly like a UI-initiated run — otherwise getChat/
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
		turns, err = st.GetTurnsWithContent(context.Background(), "quack", runUserID, sessionID)
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
// work — the fix for reviews that posted the preamble as if it were the review.
func TestHandleWebhookNudgesWhenNoPlanRan(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 4), answer: "reviewed", noPlan: true}
	ext := newTestExtension(t, runner, gh.URL)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
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
	ext2 := newTestExtension(t, planned, gh2.URL)
	rec2 := httptest.NewRecorder()
	ext2.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	select {
	case <-posted2:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back (control)")
	}
	if got := atomic.LoadInt32(&planned.calls); got != 1 {
		t.Errorf("runner invoked %d times, want 1 (a plan ran ⇒ no nudge)", got)
	}
}

// When the run submits a formal review (github_submit_review), the review IS the
// deliverable on the PR — dispatch must NOT also post the run's text summary as a
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
// previously suppressed the summary/failure comment on the CALL alone — a silent
// death with a "delivered" log line and nothing on GitHub (#286).
func TestHandleWebhookFailedDeliveryStillComments(t *testing.T) {
	posted := make(chan string, 1)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "partial progress",
		judgePassed: true, deliverErr: "github_pull_request: branch not pushed"}
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
		if !strings.Contains(body, "partial progress") {
			t.Errorf("fallback comment = %q; want the run's answer", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted after a FAILED delivery — the silent-death bug")
	}
}

// Two runs on the SAME PR session must not run concurrently — the second queues
// on the session lock until the first finishes (concurrent runs on one session
// corrupt each other).
func TestDispatchSerializesSameSession(t *testing.T) {
	posted := make(chan string, 4)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 4), answer: "ok", block: make(chan struct{})}
	ext := newTestExtensionWithTriggers(t, runner, gh.URL, []string{"mention"}, "")

	// Two mentions on issue #7 → same session. Both ack 202 and spawn a dispatch.
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
	time.Sleep(150 * time.Millisecond) // give the second a chance to (wrongly) start
	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Fatalf("runner.calls = %d while the first run holds the session lock; want 1 (the second must queue)", got)
	}

	close(runner.block) // let the first finish; the second then acquires the lock
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&runner.calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&runner.calls); got != 2 {
		t.Errorf("runner.calls = %d after releasing the lock; want 2 (the queued run should proceed)", got)
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
	time.Sleep(50 * time.Millisecond)
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
	time.Sleep(50 * time.Millisecond)
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
	time.Sleep(50 * time.Millisecond)
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

func TestRunMessageReviewAwareForPR(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this, focusing on the auth path"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := ext.runMessage(pr, "review this, focusing on the auth path", reviewContext{})
	for _, want := range []string{
		"focusing on the auth path", // user's verbatim request preserved
		"pull_number=7",             // the PR/issue number surfaced for the review tools
		"github_add_review_comment",
		"stage_review",
		"github_list_pr_comments",
		"REVIEW-ONLY", // a review carries no delivery path
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("PR review message missing %q\n---\n%s", want, msg)
		}
	}
	// A review must carry NO delivery language, or the vetting gate reads a phantom
	// commit/push demand off the node task and loops the worker (re-cloning,
	// re-reviewing) to no end. This is the regression that made a review take 30+ min.
	for _, forbidden := range []string{"stage_pr", "git_push", "commit your work"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("PR review message must not mention delivery (%q):\n%s", forbidden, msg)
		}
	}

	// With the PR's refs known, the reviewer is told to CHECK OUT the head branch —
	// without it a shallow clone's `git diff base...HEAD` is empty and it flails.
	withRefs := ext.runMessage(pr, "review this", reviewContext{meta: prMeta{HeadRef: "feat/x", HeadSHA: "abc123", BaseRef: "main"}})
	for _, want := range []string{"git_checkout `feat/x`", "git_diff main...feat/x", "is EMPTY until you check out the head"} {
		if !strings.Contains(withRefs, want) {
			t.Errorf("PR review message missing checkout guidance %q\n---\n%s", want, withRefs)
		}
	}

	// A PR request that DOES ask to change code keeps the implement path.
	impl := ext.runMessage(pr, "fix the null dereference in the auth path and open a PR", reviewContext{})
	if !strings.Contains(impl, "stage_pr") {
		t.Errorf("implement-intent PR message should keep the implement path:\n%s", impl)
	}

	// A non-PR issue stays implement-capable: no review-tool guidance.
	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack add a feature"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	imsg := ext.runMessage(issue, "add a feature", reviewContext{})
	if strings.Contains(imsg, "stage_review") || strings.Contains(imsg, "pull_number=") {
		t.Errorf("issue run message should not mention the review tools:\n%s", imsg)
	}
	if !strings.Contains(imsg, "stage_pr") {
		t.Errorf("issue run message should keep implement-path guidance:\n%s", imsg)
	}
}

func TestRunMessageChangeAwareFraming(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	tests := []struct {
		name        string
		prevSHA     string
		headSHA     string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "no prior review keeps full first-time framing",
			prevSHA:     "",
			headSHA:     "",
			wantAbsent:  []string{"previously reviewed", "Focus your review on what changed"},
			wantContain: []string{"stage_review"},
		},
		{
			name:    "prior review adds continuation framing with explicit head",
			prevSHA: "aaa111",
			headSHA: "ccc333",
			wantContain: []string{
				"previously reviewed this pull request at commit `aaa111`",
				"current head is `ccc333`",
				"git_diff aaa111..ccc333",
				"do NOT repeat findings you already made",
				"stage_review", // implement/review guidance still present
			},
		},
		{
			name:    "prior review without a known head falls back to HEAD",
			prevSHA: "aaa111",
			headSHA: "",
			wantContain: []string{
				"current head is `HEAD`",
				"git_diff aaa111..HEAD",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := ext.runMessage(pr, "review this", reviewContext{meta: prMeta{HeadSHA: tt.headSHA}, prevReviewSHA: tt.prevSHA})
			for _, want := range tt.wantContain {
				if !strings.Contains(msg, want) {
					t.Errorf("message missing %q\n---\n%s", want, msg)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(msg, absent) {
					t.Errorf("message should not contain %q\n---\n%s", absent, msg)
				}
			}
		})
	}
}

// The plan-time PR context (title, changed files, discussion) is folded into the
// run message so the orchestrator can slice the review without a node fetching it.
func TestRunMessageIncludesReviewContext(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rc := reviewContext{
		meta:  prMeta{HeadRef: "feat/x", HeadSHA: "abc123", BaseRef: "main", Title: "Add widget", Body: "This adds a widget."},
		files: []changedFile{{Filename: "a.go", Additions: 10, Deletions: 2}, {Filename: "b.go", Additions: 1}},
		discussion: prDiscussion{
			Comments:       []commentView{{User: "bob", Body: "looks good"}},
			ReviewComments: []reviewCommentView{{User: "carol", Path: "a.go", Line: 5, Body: "nit"}},
		},
	}
	msg := ext.runMessage(pr, "review this", rc)
	for _, want := range []string{
		"PR title: Add widget", "This adds a widget", // intent
		"Changed files (2)", "a.go (+10/-2)", "b.go (+1/-0)", // slicing data
		"Existing discussion", "@bob: looks good", "@carol a.go:5: nit", // don't repeat
		"git_checkout `feat/x`", // checkout guidance
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("run message missing %q\n---\n%s", want, msg)
		}
	}
}

// A conversational follow-up on a PR is answered from the session — the message
// must NOT hand over the clone-and-review playbook, or the orchestrator re-reviews
// instead of answering.
func TestRunMessageConversationalFollowup(t *testing.T) {
	ext := newTestExtension(t, &fakeRunner{}, "http://unused")
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack which finding matters most? No need to re-review."), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := ext.runMessage(pr, "which finding matters most? No need to re-review.", reviewContext{})
	for _, want := range []string{"conversational follow-up", "Answer it directly", "Do NOT clone"} {
		if !strings.Contains(msg, want) {
			t.Errorf("conversational message missing %q\n---\n%s", want, msg)
		}
	}
	for _, absent := range []string{"git_clone", "stage_review", "git_checkout"} {
		if strings.Contains(msg, absent) {
			t.Errorf("conversational message must not carry the review playbook (%q)\n---\n%s", absent, msg)
		}
	}
	// A genuine review request still gets the full playbook.
	if rev := ext.runMessage(pr, "please review this PR", reviewContext{meta: prMeta{HeadRef: "x"}}); !strings.Contains(rev, "stage_review") {
		t.Errorf("a review request must still carry the review tools:\n%s", rev)
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
	time.Sleep(50 * time.Millisecond)
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
				time.Sleep(50 * time.Millisecond)
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
	time.Sleep(50 * time.Millisecond)
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
	gh := mergeStub(t, approved, posted, merged)
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
	time.Sleep(50 * time.Millisecond)
	select {
	case <-merged:
		t.Error("merge-label sender not in allowed_users must not authorize a merge")
	default:
	}
}

// TestHandleWebhookAutoReviewIgnoresAllowlist pins that the synthetic
// pr_opened auto-review has no human invoker and fires regardless of
// allowed_users (including empty/deny-all) — the allowlist gates
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
		// AllowedUsers intentionally left unset (deny-all for human triggers) —
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
				time.Sleep(50 * time.Millisecond)
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
// ignored — the workflow is label-driven, not event-driven.
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
	time.Sleep(50 * time.Millisecond)
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
				time.Sleep(50 * time.Millisecond)
				if atomic.LoadInt32(&runner.calls) != 0 {
					t.Error("issues event should not have dispatched a run")
				}
				return
			}
			// The ack comment lands before the run.
			select {
			case c := <-posted:
				if !strings.Contains(c, "Closes #7") {
					t.Errorf("ack comment = %q, want the Closes #7 promise", c)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no ack comment posted")
			}
			select {
			case msg := <-runner.gotMessage:
				for _, want := range []string{"Implement issue #7", "Closes #7", "stage_pr", "Never merge"} {
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

// TestDispatchResetsSessionForLabelWorkRequest pins T4 session hygiene: a
// LABEL-driven work request (quack:implement) resets the session before
// running, so a new attempt is not poisoned by a prior attempt's history —
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
	case <-posted: // the "On it — implementing…" ack comment
	case <-time.After(2 * time.Second):
		t.Fatal("no ack comment posted")
	}
	select {
	case <-runner.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("implement label did not dispatch a run")
	}
	select {
	case <-posted: // the run's fallback summary comment
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

// TestImplementTaskIncludesDiscussion pins that the fetched issue comments (the
// posted plan) are embedded in the implementation request.
func TestImplementTaskIncludesDiscussion(t *testing.T) {
	var p issuesPayload
	p.Issue.Number = 7
	p.Issue.Title = "Add widget cache"
	p.Issue.Body = "Widgets are refetched on every request."
	comments := []commentView{
		{User: "quack[bot]", Body: "## Plan\n1. add lru cache to fetcher"},
		{User: "alice", Body: "looks good, approved"},
	}
	msg := implementTask(p, comments)
	for _, want := range []string{"add lru cache to fetcher", "looks good, approved", "@quack[bot]", "Closes #7", "stage_pr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("implementTask missing %q:\n%s", want, msg)
		}
	}
}

// mergeStub is stubGitHub plus a reviews list and a merge endpoint; merged
// signals when the merge PUT lands.
func mergeStub(t *testing.T, reviewsJSON string, posted chan<- string, merged chan<- struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, reviewsJSON)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			merged <- struct{}{}
			fmt.Fprint(w, `{"merged":true}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
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

func TestHandleWebhookMergeLabel(t *testing.T) {
	approved := `[{"state":"CHANGES_REQUESTED","user":{"login":"quack[bot]"}},{"state":"APPROVED","user":{"login":"quack[bot]"}}]`
	tests := []struct {
		name        string
		triggers    []string
		reviews     string
		sender      string
		wantMerge   bool
		wantComment string // substring of the posted comment; "" = no comment expected
	}{
		{"approved review merges", []string{"merge"}, approved, "alice", true, "Merged"},
		{"changes-requested refuses", []string{"merge"},
			`[{"state":"APPROVED","user":{"login":"quack[bot]"}},{"state":"CHANGES_REQUESTED","user":{"login":"quack[bot]"}}]`,
			"alice", false, "Not merging: my latest review is CHANGES_REQUESTED"},
		{"no quack review refuses", []string{"merge"},
			`[{"state":"APPROVED","user":{"login":"alice"}}]`, "alice", false, "I have not reviewed this PR"},
		{"COMMENTED carries no verdict", []string{"merge"},
			`[{"state":"APPROVED","user":{"login":"quack[bot]"}},{"state":"COMMENTED","user":{"login":"quack[bot]"}}]`,
			"alice", true, "Merged"},
		{"trigger not enabled is a no-op", []string{"mention"}, approved, "alice", false, ""},
		{"bot sender cannot authorize", []string{"merge"}, approved, "other[bot]", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 2)
			merged := make(chan struct{}, 1)
			gh := mergeStub(t, tt.reviews, posted, merged)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1)}
			ext := newTestExtensionWithTriggers(t, runner, gh.URL, tt.triggers, "")

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
				time.Sleep(50 * time.Millisecond)
				select {
				case <-merged:
					t.Error("merge must not have been called")
				default:
				}
			}
			// The merge label never dispatches an agent run.
			if atomic.LoadInt32(&runner.calls) != 0 {
				t.Error("merge label must not dispatch an orchestrator run")
			}
		})
	}
}

// TestHandleWebhookPlanLabelPostsPlanEvenWhenDelivered pins the regression where a
// plan-only run silently dropped its plan: isWorkRequest keys off the (work-verby)
// issue text, so judgePassed && isWorkRequest made `delivered` true and dispatch
// skipped the summary comment — but a plan-only run's deliverable IS that comment.
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
		t.Fatal("plan-only run did not post its plan — the delivered-skip dropped it")
	}
}

// TestHandleWebhookLabelPostsEyesReactionOnIssue pins #252: a label-triggered run
// (quack:plan / quack:implement) posts an instant 👀 on the ISSUE — POST to
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
// real issue_comment payload carries it — used to pin #380's title backfill.
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
// real, non-placeholder title derived from the triggering issue — dispatch
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
// title a prior dispatch set — mirrors runChat's own titleCh guard.
func TestDispatchDoesNotOverwriteExistingTitle(t *testing.T) {
	posted := make(chan string, 2)
	gh := stubGitHub(t, posted)
	defer gh.Close()

	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sessionID := "github-acme-widgets-7"
	if err := st.SetChatGitHub(context.Background(), sessionID, "acme/widgets", "https://github.com/acme/widgets/pull/7"); err != nil {
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

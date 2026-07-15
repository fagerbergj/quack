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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
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
		case strings.Contains(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`) // no prior review by default: first-time framing
		case strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			postedComment <- string(body)
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
	}, runner)
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
	msg := ext.runMessage(pr, "review this, focusing on the auth path", "", "")
	for _, want := range []string{
		"focusing on the auth path", // user's verbatim request preserved
		"pull_number=7",             // the PR/issue number surfaced for the review tools
		"github_add_review_comment",
		"github_submit_review",
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
	for _, forbidden := range []string{"github_pull_request", "git_push", "commit your work"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("PR review message must not mention delivery (%q):\n%s", forbidden, msg)
		}
	}

	// A PR request that DOES ask to change code keeps the implement path.
	impl := ext.runMessage(pr, "fix the null dereference in the auth path and open a PR", "", "")
	if !strings.Contains(impl, "github_pull_request") {
		t.Errorf("implement-intent PR message should keep the implement path:\n%s", impl)
	}

	// A non-PR issue stays implement-capable: no review-tool guidance.
	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack add a feature"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	imsg := ext.runMessage(issue, "add a feature", "", "")
	if strings.Contains(imsg, "github_submit_review") || strings.Contains(imsg, "pull_number=") {
		t.Errorf("issue run message should not mention the review tools:\n%s", imsg)
	}
	if !strings.Contains(imsg, "github_pull_request") {
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
			wantContain: []string{"github_submit_review"},
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
				"github_submit_review", // implement/review guidance still present
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
			msg := ext.runMessage(pr, "review this", tt.prevSHA, tt.headSHA)
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

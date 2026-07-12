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
	gotMessage chan string
	answer     string
	block      chan struct{}
	calls      int32
}

func (f *fakeRunner) Run(_ context.Context, _, _, message string, _ []*genai.Part) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		atomic.AddInt32(&f.calls, 1)
		if f.block != nil {
			<-f.block
		}
		select {
		case f.gotMessage <- message:
		default:
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
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = apiBase
	return NewExtension(app, config.GitHubExtensionConfig{WebhookSecret: testSecret, Mention: "@quack"}, runner)
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
	msg := ext.runMessage(pr, "review this, focusing on the auth path")
	for _, want := range []string{
		"focusing on the auth path", // user's verbatim request preserved
		"pull_number=7",             // the PR/issue number surfaced for the review tools
		"github_add_review_comment",
		"github_submit_review",
		"github_list_pr_comments",
		"github_pull_request", // implement-path guidance still present
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("PR run message missing %q\n---\n%s", want, msg)
		}
	}

	// A non-PR issue stays implement-only: no review-tool guidance.
	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack add a feature"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	imsg := ext.runMessage(issue, "add a feature")
	if strings.Contains(imsg, "github_submit_review") || strings.Contains(imsg, "pull_number=") {
		t.Errorf("issue run message should not mention the review tools:\n%s", imsg)
	}
	if !strings.Contains(imsg, "github_pull_request") {
		t.Errorf("issue run message should keep implement-path guidance:\n%s", imsg)
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

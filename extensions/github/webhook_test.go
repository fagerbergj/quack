package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const testSecret = "shhh-webhook-secret"

func testKeyPEM(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(pemBytes), &key.PublicKey
}

// fakeDispatchHost records every Host.Dispatch call; notify fires (non-blocking) after each so tests that
// dispatch via handleWebhook's goroutine can wait instead of polling.
type fakeDispatchHost struct {
	mu          sync.Mutex
	dispatches  []sdk.DispatchRequest
	dispatchErr error
	notify      chan sdk.DispatchRequest
	// block, if non-nil, is awaited before recording - a slow Host.Dispatch, proving handleWebhook doesn't wait on it.
	block chan struct{}

	// originUpdates records every Host.UpdateChatOrigin call. originErr, keyed
	// by localID, lets a test simulate sdk.ErrUnknownChat or any other failure.
	originUpdates []originUpdate
	originErr     map[string]error

	// invalidated records every Host.InvalidateSetup call.
	invalidated []string
}

func (f *fakeDispatchHost) invalidateSetup(chatID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, chatID)
	return nil
}

func (f *fakeDispatchHost) invalidateCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.invalidated...)
}

type originUpdate struct {
	localID string
	origin  sdk.ChatOrigin
}

func (f *fakeDispatchHost) updateChatOrigin(localID string, origin sdk.ChatOrigin) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.originUpdates = append(f.originUpdates, originUpdate{localID, origin})
	return f.originErr[localID]
}

func (f *fakeDispatchHost) originCalls() []originUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]originUpdate, len(f.originUpdates))
	copy(out, f.originUpdates)
	return out
}

func (f *fakeDispatchHost) dispatch(ctx context.Context, req sdk.DispatchRequest) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.dispatches = append(f.dispatches, req)
	f.mu.Unlock()
	if f.notify != nil {
		select {
		case f.notify <- req:
		default:
		}
	}
	return f.dispatchErr
}

func (f *fakeDispatchHost) calls() []sdk.DispatchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sdk.DispatchRequest, len(f.dispatches))
	copy(out, f.dispatches)
	return out
}

// waitForDispatch blocks for one Host.Dispatch call, failing the test if none arrives within timeout.
func (f *fakeDispatchHost) waitForDispatch(t *testing.T, timeout time.Duration) sdk.DispatchRequest {
	t.Helper()
	select {
	case req := <-f.notify:
		return req
	case <-time.After(timeout):
		t.Fatal("no Host.Dispatch call within timeout")
		return sdk.DispatchRequest{}
	}
}

// stubGitHub serves the REST endpoints dispatch touches (installation, token, comment post);
// postedComment signals when a comment lands.
func stubGitHub(t *testing.T, postedComment chan<- string) *httptest.Server {
	t.Helper()
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
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			postedComment <- string(body)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"A test issue.","state":"open"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func isIssueMetaPath(path string) bool {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "issues" {
		return false
	}
	_, err := strconv.Atoi(parts[len(parts)-1])
	return err == nil
}

func issueCommentPayloadFor(owner, repo string, number int, login, body string, apiURL string) issueCommentPayload {
	var p issueCommentPayload
	p.Action = "created"
	p.Comment.ID = 999
	p.Comment.Body = body
	p.Comment.User.Login = login
	p.Issue.Number = number
	p.Repository.Name = repo
	p.Repository.Owner.Login = owner
	p.Repository.CloneURL = "https://github.com/" + owner + "/" + repo + ".git"
	p.Repository.DefaultBranch = "main"
	p.Installation.ID = 5
	return p
}

// issueCommentBody is the raw issue_comment webhook JSON handleWebhook parses.
func issueCommentBody(commentBody string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody))
}

// pullRequestBody is the raw pull_request webhook JSON for opened/labeled actions.
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

// pullRequestStateBody is the raw pull_request webhook JSON for closed/reopened actions.
func pullRequestStateBody(action string, merged bool) []byte {
	return []byte(fmt.Sprintf(`{
		"action":%q,
		"number":7,
		"pull_request":{"merged":%t},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5},
		"sender":{"login":"alice"}
	}`, action, merged))
}

// issuesBody is the raw issues webhook JSON for the label-driven issue workflow.
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

// pullRequestReviewBody is the pull_request_review webhook payload for a submitted review.
func pullRequestReviewBody(state, reviewer string, prNumber int) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"submitted",
		"review":{"state":%q,"user":{"login":%q}},
		"pull_request":{"number":%d},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, state, reviewer, prNumber))
}

// newTestExtension wires an Extension to apiBase and a fakeDispatchHost, bypassing the sdk.Factory/YAML path.
func newTestExtension(t *testing.T, apiBase string, triggers []string) (*Extension, *fakeDispatchHost) {
	t.Helper()
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = apiBase

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	fh := &fakeDispatchHost{notify: make(chan sdk.DispatchRequest, 8)}
	host := sdk.Host{
		Dispatch:         fh.dispatch,
		UpdateChatOrigin: fh.updateChatOrigin,
		InvalidateSetup:  fh.invalidateSetup,
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataDir:          t.TempDir(),
	}

	if len(triggers) == 0 {
		triggers = []string{"mention"}
	}
	tmap := make(map[string]bool, len(triggers))
	for _, tr := range triggers {
		tmap[tr] = true
	}

	e := &Extension{
		app:      app,
		host:     host,
		store:    st,
		secret:   []byte(testSecret),
		mention:  "@quack",
		triggers: tmap,
		labels: Labels{
			Plan: "quack:plan", Implement: "quack:implement", Review: "quack-auto-review",
			Merge: "quack:merge", PartialFix: "quack:partial-fix", Fix: "quack:fix",
		},
		allowedUsers: map[string]bool{"alice": true},
		runTimeout:   time.Hour,
	}
	// Registered last so it runs first (t.Cleanup is LIFO): drain every
	// webhook-spawned goroutine before the store/server it depends on close.
	t.Cleanup(e.Wait)
	return e, fh
}

func signedRequest(event string, body []byte) *http.Request {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, webhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	return req
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

// Dispatch shapes an sdk.DispatchRequest and stores the pendingRun BEFORE calling Host.Dispatch,
// since RunEnded can fire as soon as Dispatch returns.
func TestDispatchBuildsRequestAndStoresPending(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	p := issueCommentPayloadFor("acme", "widgets", 7, "alice", "@quack take a look", srv.URL)
	e.dispatch(p, "take a look")

	calls := fh.calls()
	if len(calls) != 1 {
		t.Fatalf("Dispatch calls = %d, want 1", len(calls))
	}
	req := calls[0]
	if req.Chat.LocalID != "github-acme-widgets-7" {
		t.Errorf("LocalID = %q, want github-acme-widgets-7", req.Chat.LocalID)
	}
	if req.Chat.User != "alice" {
		t.Errorf("User = %q, want alice", req.Chat.User)
	}
	if !strings.Contains(req.Ask.Message, "<permissions>") {
		t.Errorf("Ask.Message missing <permissions> block: %q", req.Ask.Message)
	}
	if req.Run.Setup == nil || req.Run.Setup.Repo != p.Repository.CloneURL {
		t.Errorf("Run.Setup = %+v, want Repo=%q", req.Run.Setup, p.Repository.CloneURL)
	}

	chatID := globalChatID("github-acme-widgets-7")
	v, ok := e.pending.Load(chatID)
	if !ok {
		t.Fatalf("pending[%q] not stored before Dispatch returned", chatID)
	}
	if v.(*pendingRun).isReview {
		t.Error("a free-form mention must not be marked as a review run")
	}

	e.dispatch(issueCommentPayloadFor("acme", "widgets", 8, "alice", "/review", srv.URL), autoReviewTask)
	if v, ok := e.pending.Load(globalChatID("github-acme-widgets-8")); !ok || !v.(*pendingRun).isReview {
		t.Error("an autoReviewTask dispatch must be marked as a review run")
	}
}

// claimInflightFor gives chatID's stored pendingRun the live inflight lease a real dispatch leaves,
// so finalize's compare-and-delete releases it.
func claimInflightFor(t *testing.T, e *Extension, chatID, sessionID string) {
	t.Helper()
	claimedAt, _, claimed := e.claimInflight(sessionID)
	if !claimed {
		t.Fatalf("claimInflight(%q) refused a free slot", sessionID)
	}
	v, ok := e.pending.Load(chatID)
	if !ok {
		t.Fatalf("no pendingRun stored for %q", chatID)
	}
	v.(*pendingRun).claimedAt = claimedAt
}

// A second trigger for an already-dispatched session is dropped, not queued, and never reaches Host.Dispatch.
func TestDispatchDedupDropsSecondTrigger(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	p := issueCommentPayloadFor("acme", "widgets", 7, "alice", "@quack go", srv.URL)
	e.dispatch(p, "go")
	e.dispatch(p, "go again") // same session - must be deduped

	if calls := fh.calls(); len(calls) != 1 {
		t.Fatalf("Dispatch calls = %d, want 1 (second trigger should dedup)", len(calls))
	}
}

// A run that dies without settling leaves its claim behind; a claim older than the lease must be taken over
// or every later trigger dedups forever.
func TestDispatchTakesOverExpiredInflightClaim(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	// The owning run never settles: its claim just sits there, aged past the lease.
	e.inflight.Store(sessionID, time.Now().Add(-e.inflightLease()-time.Minute))

	e.dispatch(issueCommentPayloadFor("acme", "widgets", 7, "alice", "@quack go", srv.URL), "go")

	if calls := fh.calls(); len(calls) != 1 {
		t.Fatalf("Dispatch calls = %d, want 1 (an expired claim must not suppress a trigger)", len(calls))
	}
	if _, live := e.inflightActive(sessionID); !live {
		t.Error("takeover left no live claim; the new run is unprotected from its own duplicates")
	}
}

// Taking over expired claims must not weaken the guard: a trigger within a live lease is still dropped.
func TestDispatchDedupHoldsWithinLease(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	// Old, but not old enough - a genuinely long-running review.
	e.inflight.Store(sessionID, time.Now().Add(-e.inflightLease()+time.Minute))

	e.dispatch(issueCommentPayloadFor("acme", "widgets", 7, "alice", "@quack go", srv.URL), "go")

	if calls := fh.calls(); len(calls) != 0 {
		t.Fatalf("Dispatch calls = %d, want 0 (a live claim must still dedup)", len(calls))
	}
}

// A straggler from a displaced run must release only its OWN claim; deleting the takeover's claim would let
// a third trigger start a concurrent run.
func TestStragglerFinalizeKeepsTakeoverClaim(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	straggler := &pendingRun{
		sessionID: sessionID, claimedAt: time.Now().Add(-e.inflightLease() - time.Minute),
		owner: "acme", repo: "widgets", number: 7, login: "alice",
	}
	e.inflight.Store(sessionID, straggler.claimedAt)

	takeoverAt, _, claimed := e.claimInflight(sessionID)
	if !claimed {
		t.Fatal("takeover did not claim the expired slot")
	}

	e.finalize(chatID, straggler, sdk.RunOutcome{Status: sdk.RunDone, Answer: "late"})

	v, ok := e.inflight.Load(sessionID)
	if !ok {
		t.Fatal("straggler's finalize deleted the takeover's claim")
	}
	if v.(time.Time) != takeoverAt {
		t.Errorf("claim = %v, want the takeover's %v", v, takeoverAt)
	}
}

// A label-triggered run with PlanRan=false gets exactly ONE runNudge follow-up Dispatch;
// the follow-up's own RunEnded finalizes rather than nudging again.
func TestRunEndedNudgesOnceThenFinalizes(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	e.pending.Store(chatID, &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7,
		login: "alice", isLabelTrigger: true,
		dispatched: sdk.DispatchRequest{
			Run: sdk.RunConfig{Setup: &sdk.Setup{Repo: "https://github.com/acme/widgets.git", ExistingHeadRef: "pr-7-head"}},
		},
	})
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: false})

	calls := fh.calls()
	if len(calls) != 1 {
		t.Fatalf("nudge Dispatch calls = %d, want 1", len(calls))
	}
	if calls[0].Ask.Message != runNudge {
		t.Errorf("nudge message = %q, want runNudge", calls[0].Ask.Message)
	}
	// The nudge must carry the original Run.Setup, or a nudged PR review can never plan (no ExistingHeadRef).
	if calls[0].Run.Setup == nil || calls[0].Run.Setup.ExistingHeadRef != "pr-7-head" {
		t.Errorf("nudge Run.Setup = %+v, want ExistingHeadRef %q carried over from the first dispatch", calls[0].Run.Setup, "pr-7-head")
	}
	if _, ok := e.pending.Load(chatID); !ok {
		t.Fatalf("pending entry removed after nudge - RunEnded must keep it for the nudge's own callback")
	}

	// The nudge's own RunEnded fires with a real answer - finalize, not another nudge.
	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done, see the fix"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "done, see the fix") {
			t.Errorf("posted comment = %q, want it to contain the answer", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted after the nudge's own RunEnded")
	}
	if _, ok := e.pending.Load(chatID); ok {
		t.Errorf("pending entry still present after finalize")
	}
	if _, ok := e.inflight.Load(sessionID); ok {
		t.Errorf("inflight entry still present after finalize")
	}
}

// A guard hard-stop finalizes as RunFailed; nudging it would only reproduce the refused tool-call loop.
func TestRunEndedDoesNotNudgeAGuardHardStop(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	e.pending.Store(chatID, &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7,
		login: "alice", isLabelTrigger: true,
	})
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{
		Status:  sdk.RunFailed,
		PlanRan: false,
		Error:   "tool-call loop: create_plan called with identical arguments and the same result 6 times despite being refused; turn terminated",
	})

	if calls := fh.calls(); len(calls) != 0 {
		t.Fatalf("nudge Dispatch calls = %d, want 0 - a guard hard stop must not be retried", len(calls))
	}
	select {
	case body := <-posted:
		if !strings.Contains(body, "create_plan") {
			t.Errorf("posted comment = %q, want it to name the tool that kept repeating", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted for the guard-stopped run")
	}
}

// When takeDeliveryDetail reports a verified delivery, finalize must not also post the answer as a duplicate.
func TestFinalizeSkipsSummaryWhenDeliveryVerified(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice"}
	e.pending.Store(chatID, pr)
	claimInflightFor(t, e, chatID, sessionID)
	recordDelivery(chatID, deliveryOutcome{prNumber: 42, prURL: "https://github.com/acme/widgets/pull/42"})

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "I opened a PR"})

	select {
	case body := <-posted:
		t.Fatalf("unexpected summary comment posted: %q", body)
	case <-time.After(300 * time.Millisecond):
		// expected: no comment
	}
	if _, ok := e.pending.Load(chatID); ok {
		t.Errorf("pending entry still present after finalize")
	}
}

// A review run that ends done with nothing delivered must not post its analysis prose:
// the PR gets a short no-verdict note instead.
func TestFinalizeReviewWithoutVerdictPostsNote(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	e.pending.Store(chatID, &pendingRun{sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice", isPR: true, isReview: true})
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "Analysis complete. My findings: ..."})

	select {
	case body := <-posted:
		if strings.Contains(body, "Analysis complete") {
			t.Fatalf("review run posted its analysis prose: %q", body)
		}
		if !strings.Contains(body, "without a verdict") || !strings.Contains(body, "/review") {
			t.Errorf("posted comment = %q, want the no-verdict note naming /review", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted for a review run that delivered nothing")
	}
}

// When RunOutcome.Error is set on a failed run, finalize posts the real cause, not the generic silent-gap message.
func TestFinalizePostsFailureCause(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice"}
	e.pending.Store(chatID, pr)
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunFailed, PlanRan: true, Error: "model gateway returned 502 Bad Gateway"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "model gateway returned 502 Bad Gateway") {
			t.Errorf("posted comment missing the failure cause: %q", body)
		}
		if strings.Contains(body, "no answer, no error") {
			t.Errorf("posted comment should not fall back to the generic silent-gap text: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted for a failed run with a known cause")
	}
}

// TestFinalizePostsSilentGapWhenNoError pins the complement: a failed run
// with no Error and no Answer still gets the generic silent-gap message.
func TestFinalizePostsSilentGapWhenNoError(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice"}
	e.pending.Store(chatID, pr)
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunFailed, PlanRan: true})

	select {
	case body := <-posted:
		if !strings.Contains(body, "no answer, no error") {
			t.Errorf("expected the generic silent-gap comment, got: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted for a failed run with no cause")
	}
}

// A ci_fix no-op push still records a delivery; pushedSHA equal to the pre-run head must still post the answer,
// or the run's analysis posts nowhere.
func TestFinalizePostsAnswerWhenPushLeftHeadUnchanged(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice", isPR: true,
		gh: githubContext{snap: Snapshot{IsPR: true, HeadSHA: "sha-before"}},
	}
	e.pending.Store(chatID, pr)
	claimInflightFor(t, e, chatID, sessionID)
	recordDelivery(chatID, deliveryOutcome{prNumber: 42, prURL: "https://github.com/acme/widgets/pull/42", pushedSHA: "sha-before"})

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "nothing needed fixing - CI was flaky, re-run it"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "nothing needed fixing") {
			t.Errorf("posted comment missing the run's answer: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no summary comment posted for a no-op push - the run's analysis was silently dropped")
	}
}

// A push whose SHA differs from the pre-run head is real delivered work, so the duplicate comment is skipped.
func TestFinalizeSkipsSummaryWhenPushActuallyMovedHead(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice", isPR: true,
		gh: githubContext{snap: Snapshot{IsPR: true, HeadSHA: "sha-before"}},
	}
	e.pending.Store(chatID, pr)
	claimInflightFor(t, e, chatID, sessionID)
	recordDelivery(chatID, deliveryOutcome{prNumber: 42, prURL: "https://github.com/acme/widgets/pull/42", pushedSHA: "sha-after-fix"})

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "fixed the failing test"})

	select {
	case body := <-posted:
		t.Fatalf("unexpected summary comment posted for a push that actually moved the head: %q", body)
	case <-time.After(300 * time.Millisecond):
		// expected: no comment - the push itself is the delivered record
	}
}

// A cancelled run's Answer is mid-thought: finalize posts nothing, and a cancelled label run is not nudged
// since a human cancellation is not a silent no-op to retry.
func TestRunEndedCancelledPostsNoCommentAndDoesNotNudge(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	e.pending.Store(chatID, &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7,
		login: "alice", isLabelTrigger: true,
	})
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunCancelled, PlanRan: false, Answer: "halfway through drafting a reply, the user hit stop"})

	if calls := fh.calls(); len(calls) != 0 {
		t.Fatalf("Dispatch calls = %d, want 0 (a cancelled run must not be nudged)", len(calls))
	}
	select {
	case body := <-posted:
		t.Fatalf("unexpected comment posted for a cancelled run: %q", body)
	case <-time.After(300 * time.Millisecond):
		// expected: no comment
	}
	if _, ok := e.pending.Load(chatID); ok {
		t.Errorf("pending entry still present after finalize")
	}
	if _, ok := e.inflight.Load(sessionID); ok {
		t.Errorf("inflight entry still present after finalize")
	}
}

// Contrast to the cancelled case: an ordinary RunDone outcome still posts its answer as a comment.
func TestRunEndedDoneStillPostsComment(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, _ := newTestExtension(t, srv.URL, nil)

	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	e.pending.Store(chatID, &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: "alice",
	})
	claimInflightFor(t, e, chatID, sessionID)

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "finished the task"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "finished the task") {
			t.Errorf("posted comment = %q, want it to contain the answer", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted for a RunDone outcome")
	}
}

// ---- Batch A: trigger/label-routing/dispatch-shape (ported from quack's internal/github/webhook_test.go) ----

// TestHandleWebhookPROpenedTrigger pins the pr_opened trigger gate: it fires
// a dispatch only when "pr_opened" is in the configured trigger set.
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
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				req := fh.waitForDispatch(t, 2*time.Second)
				if !strings.Contains(req.Ask.Message, `<pull_request number="7">`) {
					t.Errorf("dispatch message missing the hoisted PR ask: %q", req.Ask.Message)
				}
			} else {
				select {
				case <-fh.notify:
					t.Error("pr_opened should not fire when only mention is configured")
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// Issue and PR numbers share one sequence per repo, so the chip must tell them apart;
// Href keeps GitHub's own URL spelling.
func TestChatOriginDistinguishesIssueFromPR(t *testing.T) {
	issue := chatOrigin("acme", "widgets", false, 42, "open", sdk.SubjectOpen)
	pr := chatOrigin("acme", "widgets", true, 42, "draft", sdk.SubjectOpen)

	if issue.Label == pr.Label {
		t.Errorf("issue and PR #42 share label %q; must be distinguishable without opening either", issue.Label)
	}
	if issue.Label != "acme/widgets issue#42" || issue.Kind != "issue" {
		t.Errorf("issue: label=%q kind=%q, want acme/widgets issue#42 / issue", issue.Label, issue.Kind)
	}
	if pr.Label != "acme/widgets pr#42" || pr.Kind != "pr" {
		t.Errorf("pr: label=%q kind=%q, want acme/widgets pr#42 / pr", pr.Label, pr.Kind)
	}
	if issue.Href != "https://github.com/acme/widgets/issues/42" {
		t.Errorf("issue href = %q, want .../issues/42", issue.Href)
	}
	if pr.Href != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("pr href = %q, want .../pull/42", pr.Href)
	}
	// Badge/State stay subject status only - the type lives in Kind/Label.
	if pr.Badge != "draft" || pr.State != sdk.SubjectOpen {
		t.Errorf("pr badge/state = %q/%q, want draft/open", pr.Badge, pr.State)
	}
}

// seedChatHistory stores a snapshot and review baseline for acme/widgets#7's chat.
func seedChatHistory(t *testing.T, ext *Extension) {
	t.Helper()
	ctx, chatID := context.Background(), globalChatID("github-acme-widgets-7")
	if err := ext.store.SetSnapshot(ctx, chatID, "{}"); err != nil {
		t.Fatal(err)
	}
	if err := ext.store.SetReviewBaseline(ctx, chatID, "[]"); err != nil {
		t.Fatal(err)
	}
}

// assertChatHistory checks whether acme/widgets#7's snapshot and baseline survived: a close drops both.
func assertChatHistory(t *testing.T, ext *Extension, want bool) {
	t.Helper()
	ctx, chatID := context.Background(), globalChatID("github-acme-widgets-7")
	_, snap, err := ext.store.GetSnapshot(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	_, base, err := ext.store.GetReviewBaseline(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if snap != want || base != want {
		t.Errorf("snapshot kept=%v baseline kept=%v, want both %v", snap, base, want)
	}
}

// A plain issue's close/reopen maps to the right badge and state.
func TestHandleWebhookIssueStateChangeRefreshesOrigin(t *testing.T) {
	tests := []struct {
		action    string
		wantBadge string
		wantState sdk.SubjectState
	}{
		{"closed", "closed", sdk.SubjectClosed},
		{"reopened", "open", sdk.SubjectOpen},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, nil)
			seedChatHistory(t, ext)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody(tt.action, "", "alice", false)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			assertChatHistory(t, ext, tt.action != "closed")

			calls := fh.originCalls()
			if len(calls) != 1 {
				t.Fatalf("UpdateChatOrigin calls = %d, want 1", len(calls))
			}
			if calls[0].localID != "github-acme-widgets-7" {
				t.Errorf("localID = %q, want github-acme-widgets-7 (mirrors dispatch's sessionID)", calls[0].localID)
			}
			if calls[0].origin.Badge != tt.wantBadge {
				t.Errorf("badge = %q, want %q", calls[0].origin.Badge, tt.wantBadge)
			}
			if calls[0].origin.State != tt.wantState {
				t.Errorf("state = %q, want %q", calls[0].origin.State, tt.wantState)
			}
			// A badge-only refresh rebuilds the whole origin - the type must survive it.
			if calls[0].origin.Kind != "issue" {
				t.Errorf("kind = %q, want issue", calls[0].origin.Kind)
			}
			if calls[0].origin.Label != "acme/widgets issue#7" {
				t.Errorf("label = %q, want acme/widgets issue#7", calls[0].origin.Label)
			}
			if calls[0].origin.Href != "https://github.com/acme/widgets/issues/7" {
				t.Errorf("href = %q, want .../issues/7", calls[0].origin.Href)
			}

			// A pure state change never triggers work.
			select {
			case <-fh.notify:
				t.Error("issue close/reopen should never dispatch a run")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// A PR's closed action carries merged=true/false; the badge must distinguish merged from plain closed.
func TestHandleWebhookPullRequestStateChangeRefreshesOrigin(t *testing.T) {
	tests := []struct {
		name      string
		action    string
		merged    bool
		wantBadge string
		wantState sdk.SubjectState
	}{
		{"merged", "closed", true, "merged", sdk.SubjectMerged},
		{"closed without merging", "closed", false, "closed", sdk.SubjectClosed},
		{"reopened", "reopened", false, "open", sdk.SubjectOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, nil)
			seedChatHistory(t, ext)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", pullRequestStateBody(tt.action, tt.merged)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			assertChatHistory(t, ext, tt.action != "closed")

			calls := fh.originCalls()
			if len(calls) != 1 {
				t.Fatalf("UpdateChatOrigin calls = %d, want 1", len(calls))
			}
			if calls[0].localID != "github-acme-widgets-7" {
				t.Errorf("localID = %q, want github-acme-widgets-7", calls[0].localID)
			}
			if calls[0].origin.Badge != tt.wantBadge {
				t.Errorf("badge = %q, want %q", calls[0].origin.Badge, tt.wantBadge)
			}
			if calls[0].origin.State != tt.wantState {
				t.Errorf("state = %q, want %q", calls[0].origin.State, tt.wantState)
			}
			if calls[0].origin.Kind != "pr" {
				t.Errorf("kind = %q, want pr", calls[0].origin.Kind)
			}
			if calls[0].origin.Label != "acme/widgets pr#7" {
				t.Errorf("label = %q, want acme/widgets pr#7", calls[0].origin.Label)
			}
			if calls[0].origin.Href != "https://github.com/acme/widgets/pull/7" {
				t.Errorf("href = %q, want .../pull/7", calls[0].origin.Href)
			}

			select {
			case <-fh.notify:
				t.Error("pull_request close/merge/reopen should never dispatch a run")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// sdk.ErrUnknownChat is the common case (most issues/PRs never had a chat) and must not block the webhook ack.
func TestRefreshChatOriginUnknownChatSwallowedCleanly(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)
	fh.originErr = map[string]error{"github-acme-widgets-7": sdk.ErrUnknownChat}

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("closed", "", "alice", false)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(fh.originCalls()) != 1 {
		t.Fatalf("UpdateChatOrigin calls = %d, want 1 (still attempted)", len(fh.originCalls()))
	}
}

// TestRefreshChatOriginNilHostIsNoop pins that a Host with no UpdateChatOrigin
// wired (the nil-tolerant SDK contract) never panics the webhook handler.
func TestRefreshChatOriginNilHostIsNoop(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, _ := newTestExtension(t, srv.URL, nil)
	ext.host.UpdateChatOrigin = nil

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("closed", "", "alice", false)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestHandleWebhookLabelTrigger pins the "label" trigger: it fires only for
// the configured review label, and only when "label" is in the trigger set.
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
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("labeled", tt.labelName)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				fh.waitForDispatch(t, 2*time.Second)
			} else {
				select {
				case <-fh.notify:
					t.Errorf("%s: should not have dispatched a run", tt.name)
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// TestHandleWebhookAutoReviewUsesPRSessionID pins that a pr_opened auto-review
// dispatches on the PR's own session id, not a synthetic one.
func TestHandleWebhookAutoReviewUsesPRSessionID(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"pr_opened"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	if req.Chat.LocalID != "github-acme-widgets-7" {
		t.Errorf("session id = %q; want github-acme-widgets-7", req.Chat.LocalID)
	}
}

// TestHandleWebhookMentionRespectsTriggerSet pins that a mention only
// dispatches when "mention" is in the configured trigger set.
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
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				fh.waitForDispatch(t, 2*time.Second)
			} else {
				select {
				case <-fh.notify:
					t.Error("mention should not fire when not in the trigger set")
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// Mention path end to end: the dispatch carries task/repo, and finalize posts the answer as a comment.
func TestHandleWebhookMentionTriggersRun(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	// Only the triggering comment's text is inlined; the raw event lives in the "event" artifact.
	req := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(req.Ask.Message, "add a feature") {
		t.Errorf("dispatch message missing the triggering comment's task: %q", req.Ask.Message)
	}

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done - opened PR #12"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "opened PR #12") {
			t.Errorf("posted comment missing answer: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
}

// TestHandleWebhookNoMentionIsNoop pins that triggerTask's decision is made
// synchronously, before any dispatch goroutine is spawned.
func TestHandleWebhookNoMentionIsNoop(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("just chatting, no mention")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("orchestrator should not run without a mention")
	}
}

// TestHandleWebhookUnhandledEventIsNoop pins handleWebhook's default case for
// an event type this extension does not act on.
func TestHandleWebhookUnhandledEventIsNoop(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("star", []byte(`{"action":"created"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("orchestrator should not run for an unhandled event")
	}
}

// TestHandleWebhookBadSignature pins that a tampered body fails HMAC
// verification before any dispatch happens.
func TestHandleWebhookBadSignature(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

	body := issueCommentBody("@quack do it")
	req := signedRequest("issue_comment", body)
	// Tamper with the body AFTER signing so the signature no longer matches.
	req.Body = io.NopCloser(bytes.NewReader(append(body, ' ')))

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("a bad signature must not trigger a run")
	}
}

// handleWebhook returns fast even when Host.Dispatch is slow: dispatch runs in a goroutine, never inline.
func TestHandleWebhookAcksBeforeRunFinishes(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)
	fh.block = make(chan struct{})

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
		t.Fatal("handler blocked on the dispatch instead of acking fast")
	}
	close(fh.block) // let the blocked dispatch goroutine finish
}

// TestHandleWebhookMentionPostsEyesReaction pins the instant 👀 ack, independent of the model run.
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

	ext, _ := newTestExtension(t, srv.URL, nil)

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

// TestAckReactionFailureDoesNotBlockRun pins that a failed 👀 reaction must not stop the dispatch.
func TestAckReactionFailureDoesNotBlockRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			http.Error(w, "boom", http.StatusInternalServerError) // reaction fails
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprint(w, `{"title":"Test issue","body":"","state":"open"}`)
		default:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack do it")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	// The run must still be dispatched despite the failed reaction.
	fh.waitForDispatch(t, 2*time.Second)
}

// TestHandleWebhookBotCommentIgnored pins the bot-sender guard: quack's own
// posted comments (and any other bot's) must never trigger a run.
func TestHandleWebhookBotCommentIgnored(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

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
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("a bot-authored mention must not dispatch a run")
	}
}

// A mention dispatches only when its commenter is in allowed_users (case-insensitive);
// an empty allowlist is DENY-ALL.
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
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, nil)
			ext.allowedUsers = make(map[string]bool, len(tt.allowedUsers))
			for _, u := range tt.allowedUsers {
				ext.allowedUsers[strings.ToLower(u)] = true
			}

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
				fh.waitForDispatch(t, 2*time.Second)
			} else {
				select {
				case <-fh.notify:
					t.Errorf("%s: must not dispatch a run", tt.name)
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// A label sender outside allowed_users never dispatches, even though applying the label required write access.
func TestHandleWebhookIssueLabelRespectsAllowlist(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "mallory", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("issue-labeled sender not in allowed_users must not dispatch")
	}
}

// The synthetic pr_opened auto-review has no human invoker, so it fires regardless of allowed_users.
func TestHandleWebhookAutoReviewIgnoresAllowlist(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"pr_opened"})
	ext.allowedUsers = map[string]bool{} // deny-all for human triggers; must not block auto-review

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("opened", "")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
}

// A run that persists no final answer must post an explicit failure,
// not a placeholder that reads like a run with nothing to say.
func TestHandleWebhookNoAnswerFailsLoudly(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack summarize this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: ""})

	var body string
	select {
	case body = <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}

	if strings.Contains(body, "quack finished but produced no answer.") {
		t.Errorf("posted the old silent placeholder verbatim: %q", body)
	}
	if !strings.Contains(body, "to retry") {
		t.Errorf("comment does not say what to do next: %q", body)
	}
	if !strings.Contains(strings.ToLower(body), "no error") {
		t.Errorf("comment does not describe what actually happened: %q", body)
	}
}

// A submitted formal review IS the deliverable; finalize must not also post the summary as a duplicate comment.
func TestHandleWebhookSubmittedReviewSkipsSummaryComment(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack review this PR")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "I reviewed it."})

	select {
	case body := <-posted:
		t.Errorf("a duplicate summary comment was posted despite a submitted review: %q", body)
	case <-time.After(300 * time.Millisecond):
	}
}

// A FAILED delivery is not delivered and must not fall back to the worker's self-reported answer:
// the comment is the real delivery error plus the branch, so the work is recoverable.
func TestHandleWebhookFailedDeliveryStillComments(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack implement this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	recordDelivery(chatID, deliveryOutcome{err: fmt.Errorf("github_pull_request: branch not pushed"), branch: "quack/issue-66"})
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "partial progress"})

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

// Inflight guard through handleWebhook: a second trigger before RunEnded is dropped;
// after RunEnded finalizes, a third trigger succeeds.
func TestDispatchDedupNearSimultaneousVerifiesTheInflightGuard(t *testing.T) {
	posted := make(chan string, 2)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack first")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	// Second trigger on the SAME session while the first is still in flight (no
	// RunEnded yet) - must be dropped, not queued.
	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack second")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second dispatch status = %d; want 202 (handler acks even if deduped)", rec2.Code)
	}
	select {
	case <-fh.notify:
		t.Fatal("second concurrent trigger on same sessionID should have been deduplicated")
	case <-time.After(200 * time.Millisecond):
	}

	// Finalize the first - releases the inflight claim.
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "ok"})
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never posted its answer")
	}

	// A third dispatch on the same sessionID must succeed.
	rec3 := httptest.NewRecorder()
	ext.handleWebhook(rec3, signedRequest("issue_comment", issueCommentBody("@quack third")))
	if rec3.Code != http.StatusAccepted {
		t.Fatalf("third dispatch status = %d; want 202", rec3.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	if calls := fh.calls(); len(calls) != 2 {
		t.Errorf("Host.Dispatch calls = %d, want 2 (first + third; second was deduplicated)", len(calls))
	}
}

// Dispatches on different sessions all proceed; the guard only blocks duplicate sessionIDs.
func TestDispatchDedupDifferentSessionsAllowsConcurrent(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", nil)

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
		req := fh.waitForDispatch(t, 2*time.Second)
		received[req.Chat.LocalID] = true
	}
	if !received["github-acme-widgets-7"] {
		t.Error("missing sessionID for issue #7")
	}
	if !received["github-acme-widgets-8"] {
		t.Error("missing sessionID for issue #8")
	}
}

// finalize posts a paused node's question as a comment, framed distinctly from a no-answer tail.
func TestDispatchPostsHITLCommentOnPause(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack research and advise")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	// PlanRan true: a real paused run has already executed a plan by the time
	// it hits a HITL node - see TestDispatchSkipsNudgeOnPause for the negative.
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunNeedsInput, PlanRan: true, NodeID: "node-1", Question: "What version of Go should we target?"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "Question before continuing") {
			t.Errorf("posted comment missing HITL framing: %s", body)
		}
		if !strings.Contains(body, "version of Go") {
			t.Errorf("HITL comment should carry the question: %s", body)
		}
		if strings.Contains(body, "produced no answer") {
			t.Errorf("HITL pause should NOT post the 'produced no answer' tail; got: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no HITL comment posted after the pause")
	}
}

// RunEnded does not nudge a HITL-paused run that already ran a plan.
func TestDispatchSkipsNudgeOnPause(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunNeedsInput, PlanRan: true, NodeID: "node-1", Question: "which approach?"})

	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
	if calls := fh.calls(); len(calls) != 1 {
		t.Errorf("Host.Dispatch calls = %d, want 1 (HITL pause must not trigger the nudge)", len(calls))
	}
}

// ---- deliverable classification, isWorkRequest, dispatch internals, plan collapse, title ----

func TestBuildEnvelopeDeliverableClassification(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this, focusing on the auth path"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this, focusing on the auth path", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(env, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("review-intent PR envelope missing the review deliverable:\n%s", env)
	}
	if !strings.Contains(env, `<pull_request number="7">`) {
		t.Errorf("envelope missing the hoisted pull_request ask block:\n%s", env)
	}

	// A PR request that DOES ask to change code gets the implement deliverable.
	implEnv := ext.buildEnvelope(context.Background(), pr, "fix the null dereference in the auth path and open a PR", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(implEnv, "<deliverable>a commit addressing the requested change</deliverable>") {
		t.Errorf("implement-intent PR envelope missing the implement deliverable:\n%s", implEnv)
	}

	// A non-PR issue mention never mentions review, and hoists <issue> not <pull_request>.
	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack add a feature"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	imsg := ext.buildEnvelope(context.Background(), issue, "add a feature", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(imsg, "a review with inline comments") {
		t.Errorf("issue envelope should not mention the review deliverable:\n%s", imsg)
	}
	if !strings.Contains(imsg, `<issue number="7">`) {
		t.Errorf("issue envelope missing the hoisted issue ask block:\n%s", imsg)
	}
}

// "please address these findings" fools ImplementationIntent into review-only; with "pull_request" granted,
// classifyGrantedPRDeliverable picks the deliverable and gets it right.
func TestBuildEnvelopeDeliverableClassifierResolvesFindingsAddress(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "COMMIT"}
	// PRScoped grant{PostReview,PushCommitsToPR} (no JoinPRConversation) →
	// computeGrant's PRScoped branch: postReview→"review", pushCommitsToPR→"pull_request".
	allowedKinds := []string{"review", "pull_request"}

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack please address these findings make sure they are valid first"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "please address these findings make sure they are valid first", seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a commit addressing the requested change</deliverable>") {
		t.Errorf("a findings-address request with pull_request granted should get the commit deliverable, not a second review:\n%s", env)
	}

	// Same grant, a genuine review ask still gets the review deliverable.
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "REVIEW"}
	revEnv := ext.buildEnvelope(context.Background(), pr, "take another look at the auth changes", seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(revEnv, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("a genuine review ask should still get the review deliverable:\n%s", revEnv)
	}
}

// With only "review" granted, that's the deliverable regardless of wording: classifyGrantedPRDeliverable
// is never consulted, so it cannot hand back an ungranted plan.
func TestBuildEnvelopeDeliverableBoundedBySoleGrant(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	classifier := &fakeIntentClassifier{verdict: "WORK"}
	ext.intentClassifier = classifier
	allowedKinds := []string{"review"} // PRScoped grant{PostReview: true} only, no pull_request

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack please address these findings"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "please address these findings", seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("with only review granted, the deliverable must fall back to review even though the message asks for a fix:\n%s", env)
	}
	// One call total: isWorkRequest's WORK/CONVERSATIONAL check; the review fallback makes no model call.
	if calls := atomic.LoadInt32(&classifier.calls); calls != 1 {
		t.Errorf("classifier called %d times, want 1 (deliverable choice is bounded to the sole grant, no model call needed)", calls)
	}
}

// A quack-authored PR with comment+pull_request granted, given an unambiguous change request,
// classifies as commit.
func TestBuildEnvelopeGrantedPRChangeRequestClassifiesAsCommit(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "COMMIT"}
	// PRScoped grant: pushCommitsToPR -> "pull_request", joinPRConversation -> "comment".
	allowedKinds := []string{"pull_request", "comment"}

	task := "1. EMBEDDING_MODEL should be qwen3-embed, not text-embedding-3-small. " +
		"2. GENERATOR_MODEL should be qwen3.5-9b. 3. The volume paths are wrong. " +
		"Pick one and say which. Do not silently ship a config that indexes three repos while the plan says six."
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a commit addressing the requested change</deliverable>") {
		t.Errorf("a numbered change request on a push-granted PR should classify as commit, not reply:\n%s", env)
	}
}

// Regression guard: a genuine question still gets a reply even with "pull_request" granted.
func TestBuildEnvelopeGrantedPRQuestionStaysReply(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "REPLY"}
	allowedKinds := []string{"pull_request", "comment"}

	task := "why did you vendor this instead of pulling the image?"
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a reply to their message") {
		t.Errorf("a genuine question on a push-granted PR must stay a reply, not regress to commit:\n%s", env)
	}
}

// A classifier failure here has no fallback signal, so it fails toward reply, never commit.
func TestBuildEnvelopeGrantedPRDeliverableFailsSafeToReply(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverableErr: errors.New("model unavailable")}
	allowedKinds := []string{"pull_request", "comment"}

	task := "please address these findings"
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a reply to their message") {
		t.Errorf("a classifier failure on a push-granted PR must fail safe to reply, not guess commit:\n%s", env)
	}
}

// A live REVIEW answer without "review" granted degrades to reply rather than escalating to commit.
func TestBuildEnvelopeGrantedPRDeliverableIgnoresUngrantedReview(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{grantedDeliverable: "REVIEW"}
	allowedKinds := []string{"pull_request", "comment"} // no "review"

	task := "take another look at the auth changes"
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), allowedKinds, nil)
	if !strings.Contains(env, "<deliverable>a reply to their message") {
		t.Errorf("a review verdict without review granted must degrade to reply, not surface an ungranted review deliverable:\n%s", env)
	}
}

// An issue comment asking for implementation gets the PR deliverable only when "pull_request" is granted:
// the label decides what's legal, the message what's asked.
func TestBuildEnvelopeIssueDeliverableClassification(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{issueDeliverable: "IMPLEMENT"}

	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack implement this and open the PR"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	granted := []string{"pull_request"} // issue-scoped grant{OpenPR: true}
	env := ext.buildEnvelope(context.Background(), issue, "implement this and open the PR", seedGC(Snapshot{}, 0), granted, nil)
	if !strings.Contains(env, "a pull request implementing the approved plan") {
		t.Errorf("implement request with pull_request granted should get the PR deliverable:\n%s", env)
	}

	// Same message, no quack:implement label: the grant bounds it back to a comment.
	ungranted := ext.buildEnvelope(context.Background(), issue, "implement this and open the PR", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(ungranted, "a pull request implementing") {
		t.Errorf("implement request WITHOUT pull_request granted must not surface the PR deliverable:\n%s", ungranted)
	}
	if !strings.Contains(ungranted, "an answer to their message") {
		t.Errorf("ungranted implement request should fall back to the comment deliverable:\n%s", ungranted)
	}

	// A plain question with the label still present stays a comment - the
	// classifier, not the grant alone, decides what was actually asked.
	ext.intentClassifier = &fakeIntentClassifier{issueDeliverable: "COMMENT"}
	question := ext.buildEnvelope(context.Background(), issue, "what do you think the right approach is here?", seedGC(Snapshot{}, 0), granted, nil)
	if !strings.Contains(question, "an answer to their message") {
		t.Errorf("a plain question should stay a comment even with pull_request granted:\n%s", question)
	}
}

// A classifier failure (error, timeout, unparseable) falls back to ImplementationIntent's heuristic,
// never straight to conversational.
func TestBuildEnvelopeIssueDeliverableClassifierFailureFallsBack(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{issueDeliverableErr: errors.New("model unavailable")}
	granted := []string{"pull_request"}

	var issue issueCommentPayload
	if err := json.Unmarshal(issueCommentBody("@quack implement this, commit it, and open a PR"), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// ImplementationIntent is true here, so the classifier failure must still land on the PR deliverable.
	env := ext.buildEnvelope(context.Background(), issue, "implement this, commit it, and open a PR", seedGC(Snapshot{}, 0), granted, nil)
	if !strings.Contains(env, "a pull request implementing the approved plan") {
		t.Errorf("classifier failure should fall back to ImplementationIntent's reading, not conversational:\n%s", env)
	}

	// A message with no delivery wording falls back to the heuristic's negative reading too.
	plain := ext.buildEnvelope(context.Background(), issue, "what do you think?", seedGC(Snapshot{}, 0), granted, nil)
	if !strings.Contains(plain, "an answer to their message") {
		t.Errorf("classifier failure on a non-implement message should still fall back to the comment deliverable:\n%s", plain)
	}
}

// A new session seeds the whole thread as <comments count="N">, minus the triggering comment
// (already quoted in <event>).
func TestBuildEnvelopeSeedsFullOnFirstLoad(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
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
	env := ext.buildEnvelope(context.Background(), issue, "rework it - mem0 is not a store", seedGC(snap, issue.Comment.ID), nil, nil)
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

// A resumed session seeds only new/edited/deleted comments, never the whole thread again.
func TestBuildEnvelopeResumeSeedsOnlyDelta(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var issue issueCommentPayload
	issue.Issue.Number = 7

	old := Snapshot{Body: "desc", Comments: []snapshotComment{{ID: 1, User: "bob", Body: "first comment", CreatedAt: "t0"}}}
	cur := Snapshot{Body: "desc", Comments: []snapshotComment{
		{ID: 1, User: "bob", Body: "first comment", CreatedAt: "t0"},
		{ID: 2, User: "carol", Body: "a brand new comment", CreatedAt: "t1"},
	}}
	delta := diffSnapshots(old, cur, 0)
	env := ext.buildEnvelope(context.Background(), issue, "what's new?", githubContext{snap: cur, delta: &delta}, nil, nil)
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
	noopEnv := ext.buildEnvelope(context.Background(), issue, "anything new?", githubContext{snap: cur, delta: &unchanged}, nil, nil)
	if strings.Contains(noopEnv, "a brand new comment") || strings.Contains(noopEnv, "first comment") {
		t.Errorf("an unchanged-snapshot resume should inject no comment content:\n%s", noopEnv)
	}
}

// <changed_files> is seeded on PR runs only, in GitHub's own filename/additions/deletions shape.
func TestBuildEnvelopeChangedFilesOnPRRuns(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	snap := Snapshot{
		IsPR:  true,
		Files: []changedFile{{Filename: "a.go", Additions: 10, Deletions: 2}, {Filename: "b.go", Additions: 1}},
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(snap, 0), nil, nil)
	if !strings.Contains(env, `<changed_files count="2" additions="11" deletions="2">`) {
		t.Errorf("envelope missing the changed_files summary attributes:\n%s", env)
	}
	if !strings.Contains(env, `"filename":"a.go"`) || !strings.Contains(env, `"additions":10`) {
		t.Errorf("envelope missing per-file churn in GitHub's own field names:\n%s", env)
	}

	var issue issueCommentPayload
	issue.Issue.Number = 7
	issueEnv := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, nil)
	if strings.Contains(issueEnv, "<changed_files") {
		t.Errorf("an issue-scoped envelope should carry no changed_files block:\n%s", issueEnv)
	}
}

// A resume with new commits gets the "what's new" deliverable; with none, no full review is owed either.
func TestBuildEnvelopeIncrementalReviewScoping(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// First-time review: no prior baseline, the full-review deliverable.
	first := ext.buildEnvelope(context.Background(), pr, "review this", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(first, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("first-time review should get the full-review deliverable:\n%s", first)
	}

	// Resume with new commits: the incremental deliverable, naming the SHA.
	withNew := ext.buildEnvelope(context.Background(), pr, "review this", githubContext{
		snap:       Snapshot{IsPR: true},
		newCommits: []snapshotCommit{{SHA: "abc1234567", Message: "fix the bug"}},
	}, nil, nil)
	if !strings.Contains(withNew, "a review of what is new since the last one") || !strings.Contains(withNew, "abc1234") {
		t.Errorf("incremental review envelope missing the scoped deliverable naming the new commit:\n%s", withNew)
	}

	// Resume with zero new commits still reads as "scoped to what's new" (a
	// review baseline exists), not the first-time framing.
	noneNew := ext.buildEnvelope(context.Background(), pr, "review this", githubContext{snap: Snapshot{IsPR: true}, newCommits: []snapshotCommit{}}, nil, nil)
	if !strings.Contains(noneNew, "already looked at every commit") {
		t.Errorf("zero-new-commits resume should say there's nothing new, not the first-time framing:\n%s", noneNew)
	}
}

// A PR mention classified CONVERSATIONAL gets the reply deliverable; a real work request gets the work one.
func TestBuildEnvelopeConversationalFollowup(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack which finding matters most? No need to re-review."), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "which finding matters most? No need to re-review.", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(env, "<deliverable>a reply to their message, posted as a comment") {
		t.Errorf("conversational envelope missing the reply deliverable:\n%s", env)
	}

	// A genuine review request, classified as a work request, gets the work deliverable.
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	rev := ext.buildEnvelope(context.Background(), pr, "please review this PR", seedGC(Snapshot{IsPR: true, HeadRef: "x"}, 0), nil, nil)
	if !strings.Contains(rev, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("a classified work request must still get the review deliverable:\n%s", rev)
	}
}

// TestBuildEnvelopeMentionClassifiedAsWork/Conversational pin that the
// classifier's verdict (not task wording) decides the deliverable.
func TestBuildEnvelopeMentionClassifiedAsWork(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "WORK"}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this, focusing on the auth path"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "review this, focusing on the auth path", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if strings.Contains(env, "a reply to their message") {
		t.Errorf("a mention classified WORK should not get the conversational deliverable:\n%s", env)
	}
}

func TestBuildEnvelopeMentionClassifiedAsConversational(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "CONVERSATIONAL"}
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack what did you mean by that finding?"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, "what did you mean by that finding?", seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(env, "a reply to their message") {
		t.Errorf("a mention classified CONVERSATIONAL should get the reply deliverable:\n%s", env)
	}
}

// A label trigger is work by construction, so buildEnvelope must never call the classifier for it.
func TestBuildEnvelopeLabelTriggerNeverClassifies(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	classifier := &fakeIntentClassifier{verdict: "CONVERSATIONAL"} // even a "no" verdict must not flip a label trigger
	ext.intentClassifier = classifier

	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody("@quack review this"), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pr.isLabelTrigger = true
	env := ext.buildEnvelope(context.Background(), pr, autoReviewTask, seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if strings.Contains(env, "a reply to their message") {
		t.Errorf("a label-triggered PR request should never get the conversational deliverable:\n%s", env)
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 0 {
		t.Errorf("classifier called %d times for a label trigger, want 0 (work by construction)", calls)
	}
}

// quack:partial-fix suppresses the Closes keyword, read off the freshly fetched snapshot labels.
func TestBuildEnvelopePartialFixOmitsClosesKeyword(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.labels.PartialFix = "quack:partial-fix"
	var issue issueCommentPayload
	issue.Issue.Number = 42
	issue.isLabelTrigger = true

	full := ext.buildEnvelope(context.Background(), issue, "implement it", seedGC(Snapshot{Labels: []string{"quack:implement"}}, 0), nil, nil)
	if !strings.Contains(full, "Closes #42") {
		t.Errorf("a non-partial implement envelope should ask for a Closes keyword:\n%s", full)
	}

	partial := ext.buildEnvelope(context.Background(), issue, "implement it", seedGC(Snapshot{Labels: []string{"quack:implement", "quack:partial-fix"}}, 0), nil, nil)
	if strings.Contains(partial, "Closes #42") {
		t.Errorf("a partial-fix envelope must not ask for a Closes keyword:\n%s", partial)
	}
	if !strings.Contains(partial, "partial fix") {
		t.Errorf("a partial-fix envelope should say so:\n%s", partial)
	}
}

// Plan-only deliverable, with the issue body appearing exactly once (only in <issue><description>).
func TestBuildEnvelopePlanOnlyDeliverable(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
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

	env := ext.buildEnvelope(context.Background(), synthetic, task, seedGC(Snapshot{Body: body}, 0), nil, nil)

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

// Small models wrap their verdict ("**WORK**"); exact matching would fail safe to conversational and lose
// review framing. CONVERSATIONAL wins when a hedged answer names both.
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
			ext, _ := newTestExtension(t, "http://unused", nil)
			ext.intentClassifier = &fakeIntentClassifier{verdict: tt.answer}
			if got := ext.isWorkRequest(context.Background(), issueCommentPayload{}, "@quack review this"); got != tt.want {
				t.Errorf("isWorkRequest(%q) = %v, want %v", tt.answer, got, tt.want)
			}
		})
	}
}

func TestIsWorkRequestFailsSafe(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)

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
			ext.intentClassifier = c.classifier
			if ext.isWorkRequest(context.Background(), issueCommentPayload{}, "review this PR") {
				t.Errorf("isWorkRequest = true, want false (fail safe to conversational)")
			}
		})
	}
}

// prWithReviewLabel builds a minimal payload carrying the configured review label.
func prWithReviewLabel() issueCommentPayload {
	var p issueCommentPayload
	p.Issue.Labels = []struct {
		Name string `json:"name"`
	}{{Name: "quack-auto-review"}}
	return p
}

// fallbackNoticeServer stubs the GitHub REST calls a fallback reaction needs
// (auth + POST .../reactions) and records every reaction body posted.
func fallbackNoticeServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reactions"):
			body, _ := io.ReadAll(r.Body)
			posted = append(posted, string(body))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &posted
}

// A classifier failure on a PR carrying the review label defaults to review, marked by a reaction
// (no comment: the run's answer is the trigger's one comment).
func TestIsWorkRequestFallbackDefaultsToReviewOnReviewLabel(t *testing.T) {
	srv, posted := fallbackNoticeServer(t)
	ext, _ := newTestExtension(t, srv.URL, nil)
	ext.intentClassifier = &fakeIntentClassifier{errAlways: errors.New("model unavailable")}

	p := prWithReviewLabel()
	p.Comment.ID = 1
	if !ext.isWorkRequest(context.Background(), p, "what do you think?") {
		t.Error("isWorkRequest = false, want true (review label present, classifier failed)")
	}
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "confused") {
		t.Errorf("fallback reaction not posted as expected, got %v", *posted)
	}
}

// A bare "re-review"/"review again" mention without the label must not fall back to conversational either.
func TestIsWorkRequestFallbackDefaultsToReviewOnBareReRunPhrase(t *testing.T) {
	srv, posted := fallbackNoticeServer(t)
	ext, _ := newTestExtension(t, srv.URL, nil)

	var p issueCommentPayload
	p.Comment.ID = 1
	for _, task := range []string{"re-review", "  review again  ", "Re-Review."} {
		t.Run(task, func(t *testing.T) {
			ext.intentClassifier = &fakeIntentClassifier{verdict: "gibberish"} // unparseable
			*posted = nil
			if !ext.isWorkRequest(context.Background(), p, task) {
				t.Errorf("isWorkRequest(%q) = false, want true (bare re-run phrase)", task)
			}
			if len(*posted) != 1 || !strings.Contains((*posted)[0], "confused") {
				t.Errorf("fallback reaction not posted as expected, got %v", *posted)
			}
		})
	}

	// A re-review ask with extra words is NOT bare - goes through the model, no forced fallback.
	ext.intentClassifier = &fakeIntentClassifier{verdict: "CONVERSATIONAL"}
	*posted = nil
	if ext.isWorkRequest(context.Background(), issueCommentPayload{}, "please re-review this when you get a chance") {
		t.Error("isWorkRequest = true, want false (non-bare phrasing goes through the classifier)")
	}
	if len(*posted) != 0 {
		t.Errorf("no fallback expected when the classifier answered cleanly, got %v", *posted)
	}
}

// With no review signal, the conversational fallback must still be marked, not silent.
func TestIsWorkRequestFallbackPostsNoticeWithoutReviewSignal(t *testing.T) {
	srv, posted := fallbackNoticeServer(t)
	ext, _ := newTestExtension(t, srv.URL, nil)
	ext.intentClassifier = &fakeIntentClassifier{verdict: "not sure"}

	var p issueCommentPayload
	p.Comment.ID = 1
	if ext.isWorkRequest(context.Background(), p, "what do you think about this?") {
		t.Error("isWorkRequest = true, want false (no review signal present)")
	}
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "confused") {
		t.Errorf("fallback reaction not posted as expected, got %v", *posted)
	}
}

// sequencedIntentClassifier returns errAlways on its first N calls, then verdict.
type sequencedIntentClassifier struct {
	failCalls int32
	verdict   string
	calls     int32
}

func (s *sequencedIntentClassifier) Classify(_ context.Context, _ string) (string, error) {
	n := atomic.AddInt32(&s.calls, 1)
	if n <= s.failCalls {
		return "", errors.New("timed out")
	}
	return s.verdict, nil
}

// A classifier call that fails once (e.g. cold model) gets one longer-deadline retry before any fallback.
func TestIsWorkRequestRetriesOnceBeforeFallingBack(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	classifier := &sequencedIntentClassifier{failCalls: 1, verdict: "WORK"}
	ext.intentClassifier = classifier

	if !ext.isWorkRequest(context.Background(), issueCommentPayload{}, "review this PR") {
		t.Error("isWorkRequest = false, want true (second attempt succeeded)")
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 2 {
		t.Errorf("classifier called %d times, want exactly 2 (one retry)", calls)
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
	ext, _ := newTestExtension(t, "http://unused", nil)
	ext.intentClassifier = blockingIntentClassifier{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if ext.isWorkRequest(ctx, issueCommentPayload{}, "review this PR") {
		t.Error("isWorkRequest = true on timeout, want false (fail safe to conversational)")
	}
}

// delayedIntentClassifier is a ctx-aware model call that takes delay to answer, racing the deadline.
type delayedIntentClassifier struct {
	calls   int32
	delay   time.Duration
	verdict string
}

func (d *delayedIntentClassifier) Classify(ctx context.Context, _ string) (string, error) {
	atomic.AddInt32(&d.calls, 1)
	select {
	case <-time.After(d.delay):
		return d.verdict, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// A 15s classifier answer (past the 5s first attempt, inside the 30s retry) must still land:
// the pre-dispatch pipeline must not share the 10s reaction budget.
func TestDispatchGivesClassifierRetryItsFullBudget(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)

	classifier := &delayedIntentClassifier{delay: 15 * time.Second, verdict: "WORK"}
	e.intentClassifier = classifier

	task := "what do you think of this approach?"
	var p issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	e.dispatch(p, task)

	calls := fh.calls()
	if len(calls) != 1 {
		t.Fatalf("Dispatch calls = %d, want 1", len(calls))
	}
	if strings.Contains(calls[0].Ask.Message, "<deliverable>a reply to their message") {
		t.Errorf("classifier's WORK verdict was discarded; dispatch fell back to conversational:\n%s", calls[0].Ask.Message)
	}
	select {
	case msg := <-posted:
		t.Errorf("fallbackWorkRequest fired (retry budget was clamped): %q", msg)
	default:
	}
}

// A method call quoted in code (it.migrate(connection)) is a correction, not an instruction;
// the deliverable must follow the classifier's CONVERSATIONAL verdict end to end.
func TestBuildEnvelopeQuotedCodeCorrectionNotWorkRequest(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)
	classifier := &fakeIntentClassifier{verdict: "CONVERSATIONAL"}
	ext.intentClassifier = classifier

	task := "That finding was wrong - it.migrate(connection) is called during setup, not teardown."
	var pr issueCommentPayload
	if err := json.Unmarshal(pullCommentBody(task), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env := ext.buildEnvelope(context.Background(), pr, task, seedGC(Snapshot{IsPR: true}, 0), nil, nil)
	if !strings.Contains(env, "a reply to their message") {
		t.Errorf("a correction quoting code must be conversational, not a work request:\n%s", env)
	}
	if calls := atomic.LoadInt32(&classifier.calls); calls != 1 {
		t.Errorf("classifier called %d times, want exactly 1 for a mention", calls)
	}
}

// End to end on the real sqlite store: dispatch 1 seeds full context; after a new GitHub comment,
// dispatch 2 injects only that comment.
func TestDispatchFirstLoadSeedsThenResumeInjectsDelta(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[{"id":1,"body":"the original comment","user":{"login":"bob"},"updated_at":"t0"}]`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack what do you think?")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}
	firstReq := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(firstReq.Ask.Message, "the original comment") {
		t.Errorf("first (seed) dispatch missing the existing comment:\n%s", firstReq.Ask.Message)
	}
	if !strings.Contains(firstReq.Ask.Message, "Should we use widgets?") {
		t.Errorf("first (seed) dispatch missing the issue body:\n%s", firstReq.Ask.Message)
	}
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "ok"})

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
	secondReq := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(secondReq.Ask.Message, "a brand-new follow-up") {
		t.Errorf("resume dispatch missing the new comment:\n%s", secondReq.Ask.Message)
	}
	if strings.Contains(secondReq.Ask.Message, "the original comment") {
		t.Errorf("resume dispatch re-injected the UNCHANGED comment - should carry only the delta:\n%s", secondReq.Ask.Message)
	}
}

// Review scope keys off the commits a review was DELIVERED at, not the general snapshot that every
// dispatch advances: a conversational run between reviews must not hide new commits.
func TestReviewBaselineDecoupledFromGeneralSnapshot(t *testing.T) {
	// Two synthetic commits with real, distinct git patch-ids (gitPatchID
	// reads a diff from stdin - no clone needed, see snapshot.go).
	diffs := map[string]string{
		"c1": "diff --git a/f1.txt b/f1.txt\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/f1.txt\n@@ -0,0 +1 @@\n+c1\n",
		"c2": "diff --git a/f2.txt b/f2.txt\nnew file mode 100644\nindex 0000000..2222222\n--- /dev/null\n+++ b/f2.txt\n@@ -0,0 +1 @@\n+c2\n",
	}
	var commitsJSON atomic.Value
	commitsJSON.Store(`[{"sha":"c1","commit":{"message":"add f1"}}]`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app": // botLogin, called computing this run's permission grant
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
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	run := func(task, verdict string, reviewDelivered bool) string {
		t.Helper()
		ext.intentClassifier = &fakeIntentClassifier{verdict: verdict}
		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack "+task)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch status = %d; want 202 (task=%q)", rec.Code, task)
		}
		req := fh.waitForDispatch(t, 2*time.Second)
		chatID := globalChatID("github-acme-widgets-7")
		if reviewDelivered {
			recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
		}
		ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "handled"})
		return req.Ask.Message
	}

	// 1. First review ever: full review (no baseline yet), and it DELIVERS -
	// the baseline should advance to just c1's patch-id.
	first := run("review this", "WORK", true)
	if strings.Contains(first, "Focus your review on what's NEW") || strings.Contains(first, "already looked at every commit") {
		t.Errorf("first-ever review should carry no incremental scoping language:\n%s", first)
	}

	// 2. c2 lands on the PR.
	commitsJSON.Store(`[{"sha":"c1","commit":{"message":"add f1"}},{"sha":"c2","commit":{"message":"add f2"}}]`)

	// 3. A CONVERSATIONAL dispatch advances the general snapshot but must not touch the review baseline.
	_ = run("what do you think so far? no need to re-review", "CONVERSATIONAL", false)

	// 4. A review request must still see c2 as new; keyed off the general snapshot it would read as seen.
	second := run("review this", "WORK", true)
	if !strings.Contains(second, "Focus your review on what's NEW") || !strings.Contains(second, "c2") {
		t.Errorf("review after a conversational dispatch must still scope to c2:\n%s", second)
	}
	if strings.Contains(second, "already looked at every commit") {
		t.Errorf("review under-scoped itself off the general snapshot instead of the review baseline:\n%s", second)
	}

	// 5. Step 4 delivered a review covering c2, so the baseline advances and the next review sees nothing new.
	third := run("review this", "WORK", false)
	if !strings.Contains(third, "already looked at every commit") {
		t.Errorf("after the review in step 4 delivered, the next review should see zero new commits:\n%s", third)
	}
}

// Own-PR reviews submit as COMMENTED (GitHub forbids approving your own PR) with the verdict in a hidden
// marker; latestQuackVerdict must read the marker, not the state.
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
	ext := &Extension{app: app}

	ref, err := ext.latestQuackVerdict(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("latestQuackVerdict: %v", err)
	}
	if ref.verdict != "approve" {
		t.Errorf("verdict = %q; want %q (from the review body marker, not its COMMENTED state)", ref.verdict, "approve")
	}
}

// A label-driven implement run's Setup is deterministic: clone_url, default-branch base,
// quack/issue-<N> work branch.
func TestDispatchAttachesDeterministicGitHubSetup(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 4))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	setup := req.Run.Setup
	if setup == nil {
		t.Fatal("no deterministic Setup attached to the dispatch request")
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

// A label-driven work request resets history so a retry isn't poisoned by the prior attempt;
// an @mention keeps history (see TestDispatchDoesNotResetSessionForMention).
func TestDispatchResetsSessionForLabelWorkRequest(t *testing.T) {
	posted := make(chan string, 4)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	req := fh.waitForDispatch(t, 2*time.Second)
	if !req.Chat.ResetHistory {
		t.Error("ResetHistory = false for a label-driven work request; want true")
	}

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done"})
	select {
	case body := <-posted:
		if !strings.Contains(body, "done") {
			t.Errorf("summary comment = %q; want the run's answer", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no summary comment posted")
	}
}

func TestDispatchDoesNotResetSessionForMention(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack implement a feature")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	req := fh.waitForDispatch(t, 2*time.Second)
	if req.Chat.ResetHistory {
		t.Error("ResetHistory = true for a conversational @mention; want false (needs continuity)")
	}

	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done"})
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted back")
	}
}

// A persistently failing issueMeta flags the context UNAVAILABLE, not an empty "valid" snapshot
// indistinguishable from a legitimately empty issue.
func TestFetchSnapshotRequiredMetaFailureSurfacesAsUnusable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	ext, _ := newTestExtension(t, srv.URL, nil)
	got := ext.loadGithubContext(context.Background(), "sess-1", "acme", "widgets", 7, false, 0, false)
	if !got.contextUnavailable {
		t.Error("contextUnavailable = false; want true when the required meta fetch fails")
	}
	if !got.firstLoad {
		t.Error("firstLoad = false; want true (no snapshot to diff against)")
	}
}

// A label-triggered implement with unloadable GitHub context must not dispatch;
// it aborts with an honest comment.
func TestDispatchAbortsLabelImplementWhenContextUnavailable(t *testing.T) {
	posted := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			// A transient 503 that persists past the retry budget.
			http.Error(w, `{"message":"No server is currently available to service your request."}`, http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var abortComment string
	select {
	case abortComment = <-posted:
	case <-time.After(5 * time.Second):
		t.Fatal("no abort comment posted")
	}
	if !strings.Contains(abortComment, "not running") || !strings.Contains(abortComment, "Re-apply the label") {
		t.Errorf("abort comment = %q; want the don't-run-blind message", abortComment)
	}

	// Host.Dispatch must never have been called - must not implement blind.
	select {
	case <-fh.notify:
		t.Error("Host.Dispatch was called; want no dispatch when context is unavailable")
	case <-time.After(200 * time.Millisecond):
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Errorf("Host.Dispatch calls = %d; want 0 (must not implement blind)", len(calls))
	}
}

// A new plan minimizes any prior quack plan comment (by its marker) via GraphQL,
// so the thread shows only the current plan.
func TestDispatchCollapsesPriorPlanComment(t *testing.T) {
	posted := make(chan string, 1)
	var minimizedID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			w.WriteHeader(http.StatusCreated) // the label-triggered 👀 ack; irrelevant here
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
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## New Plan\n1. do the thing"})

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

// A plan requested via a /quack comment (not the label) still carries the plan marker on its tail comment.
func TestDispatchMarksCommentTriggeredPlan(t *testing.T) {
	posted := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## The Plan\n1. do the thing"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("comment-triggered plan comment = %q; want it to carry the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted")
	}
}

// deliverableText (envelope and worker ask) and deliverableIsPlan share one memoized classifier answer;
// a live model can disagree with itself, so a second call could drop the plan marker.
func TestDispatchClassifiesIssueDeliverableOnce(t *testing.T) {
	posted := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	classifier := &fakeIntentClassifier{issueDeliverable: "COMMENT"}
	ext, fh := newTestExtension(t, srv.URL, nil)
	ext.intentClassifier = classifier

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## The Plan\n1. do the thing"})

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

// Two successive comment-triggered plans: the first is minimized before the second posts.
func TestDispatchCollapsesPriorCommentTriggeredPlan(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[]`)
	posted := make(chan string, 2)
	var minimizedID atomic.Value
	minimizedID.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)
	chatID := globalChatID("github-acme-widgets-7")

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d; want 202", rec1.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## First Plan\n1. step one"})
	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Fatalf("first plan comment = %q; want the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no first plan comment posted")
	}

	// The first plan is on GitHub, marker and all; the second run's collapse must find it.
	commentsJSON.Store(`[{"id":11,"node_id":"PLAN1","body":"## First Plan\n1. step one\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)

	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issue_comment", issueCommentBody("@quack plan this again")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second dispatch status = %d; want 202", rec2.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## Second Plan\n1. step two"})
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

// A comment-triggered plan followed by a label replan is still minimized,
// which only works because the first run carried the marker.
func TestDispatchCollapsesCommentTriggeredPlanOnLabelReplan(t *testing.T) {
	var commentsJSON atomic.Value
	commentsJSON.Store(`[]`)
	posted := make(chan string, 2)
	var minimizedID atomic.Value
	minimizedID.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, []string{"mention", "issue_plan"})
	chatID := globalChatID("github-acme-widgets-7")

	// First: a plain /quack comment asks for a plan.
	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("comment-triggered dispatch status = %d; want 202", rec1.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## Comment-Triggered Plan\n1. step one"})
	select {
	case body := <-posted:
		if !strings.Contains(body, "quack:delivery:plan") {
			t.Fatalf("comment-triggered plan comment = %q; want the plan delivery marker", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no comment-triggered plan comment posted")
	}

	commentsJSON.Store(`[{"id":11,"node_id":"PLAN1","body":"## Comment-Triggered Plan\n1. step one\n\n<!-- quack:delivery:plan -->","user":{"login":"quack[bot]"}}]`)

	// Second: a maintainer applies quack:plan to re-plan properly.
	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("label-triggered dispatch status = %d; want 202", rec2.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## Labeled Plan\n1. step two"})
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

// A non-plan deliverable's tail comment carries no plan marker and triggers no collapse.
func TestDispatchImplementRunUntouchedByPlanCollapse(t *testing.T) {
	posted := make(chan string, 1)
	var graphqlCalled int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, []string{"issue_implement"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:implement", "alice", false)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "implemented the change"})

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

// Collapse is best-effort: a minimizeComment failure must not block the new plan's delivery.
func TestDispatchPostsPlanWhenCollapseFails(t *testing.T) {
	posted := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack plan this issue")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	chatID := globalChatID("github-acme-widgets-7")
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## New Plan\n1. do the thing"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "New Plan") || !strings.Contains(body, "quack:delivery:plan") {
			t.Errorf("plan comment = %q; want it posted with its marker despite the collapse failure", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no plan comment posted - a collapse failure must not block delivery")
	}
}

// planTask must not embed the issue body; the envelope already carries it verbatim.
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
		t.Errorf("planTask embeds the issue body itself - the envelope's context block already carries it:\n%s", msg)
	}
}

// implementTask contributes the issue number/title and delivery instructions;
// the approved plan arrives via loadGithubContext.
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

	// A custom configured partial-fix label is honoured, not a hardcoded default.
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

// mentionCommentBody is issueCommentBody with the issue's title present, as a real payload carries it.
func mentionCommentBody(commentBody, issueTitle string) []byte {
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"}},
		"issue":{"number":7,"title":%q},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, commentBody, issueTitle))
}

// A webhook-dispatched chat gets a real title from the triggering issue via Chat.Title
// (the host applies it only if the chat has none).
func TestDispatchGeneratesTitle(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", mentionCommentBody("@quack review this", "Widgets leak memory")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	if req.Chat.Title == "" || req.Chat.Title == "New chat" {
		t.Errorf("Title = %q; want a real title derived from the issue", req.Chat.Title)
	}
	if req.Chat.Title != "Widgets leak memory" {
		t.Errorf("Title = %q; want the issue title", req.Chat.Title)
	}
}

// Title for the label-driven path, whose payload is synthesized from an issuesPayload.
func TestDispatchTitleFromLabelDrivenIssue(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	if req.Chat.Title != "Add widget cache" {
		t.Errorf("Title = %q; want the issue title from issuesBody", req.Chat.Title)
	}
}

// ---- merge-label / issue-label / request-changes batch (ported from quack's internal/github/webhook_test.go) ----

// newTestExtensionWithStore reuses an existing *ghStore, simulating a restart over the same durable state.
func newTestExtensionWithStore(t *testing.T, apiBase string, triggers []string, st *ghStore) (*Extension, *fakeDispatchHost) {
	t.Helper()
	e, fh := newTestExtension(t, apiBase, triggers)
	e.store = st
	return e, fh
}

// RunEnded -> finalize -> tryMerge with the claim held: a moved head keeps the standing intent, posts nothing,
// and the re-review dispatch is not dedup-dropped. A failing metaStatus covers the title lookup failing.
func TestFinalizeReReviewsOnHeadModified(t *testing.T) {
	const newHeadSHA = "newhead2"
	for _, tc := range []struct {
		name       string
		metaStatus int
	}{
		{"pull meta ok", http.StatusOK},
		// 404 not 500: the GET transport retries 5xx, which would mask the failure.
		{"pull meta fails: still re-reviews", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posted := make(chan string, 8)
			var metaCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/installation"):
					fmt.Fprint(w, `{"id":5}`)
				case strings.HasSuffix(r.URL.Path, "/access_tokens"):
					fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
				case strings.HasSuffix(r.URL.Path, "/app"):
					fmt.Fprint(w, `{"slug":"quack"}`)
				case strings.HasSuffix(r.URL.Path, "/reviews"):
					// commit_id "oldhead1" != the PR's current head (newHeadSHA): "moved" must come from this mismatch,
					// not a stale dispatch-time snapshot.
					fmt.Fprint(w, `[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"},"commit_id":"oldhead1"}]`)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
					fmt.Fprint(w, `[]`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
					body, _ := io.ReadAll(r.Body)
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{}`)
					posted <- string(body)
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
					t.Errorf("mergePR called; the commit_id/head mismatch must be caught before ever attempting a merge")
				case strings.HasSuffix(r.URL.Path, "/files"):
					fmt.Fprint(w, `[]`)
				case strings.HasSuffix(r.URL.Path, "/commits"):
					fmt.Fprint(w, `[]`)
				case strings.Contains(r.URL.Path, "/check-runs"):
					fmt.Fprint(w, `{"check_runs":[]}`)
				case strings.Contains(r.URL.Path, "/pulls/"):
					// The FIRST pull lookup is tryMerge's head fetch; the SECOND is the re-dispatch's snapshot fetch,
					// which must see a healthy PR or the label path aborts as blind.
					if metaCalls.Add(1) == 2 {
						w.WriteHeader(tc.metaStatus)
					}
					fmt.Fprintf(w, `{"title":"Test PR","body":"","state":"open","head":{"ref":"feature-branch","sha":%q},"base":{"ref":"main"}}`, newHeadSHA)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			e, fh := newTestExtension(t, srv.URL, []string{"merge"})
			sessionID := "github-acme-widgets-7"
			chatID := globalChatID(sessionID)
			pr := &pendingRun{
				sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: autoReviewUser, isPR: true,
				gh:            githubContext{snap: Snapshot{IsPR: true, HeadSHA: "oldhead1", BaseRef: "main"}},
				dispatched:    sdk.DispatchRequest{Run: sdk.RunConfig{Setup: &sdk.Setup{Repo: "https://github.com/acme/widgets.git"}}},
				defaultBranch: "main", installationID: 5,
			}
			e.pending.Store(chatID, pr)
			claimInflightFor(t, e, chatID, sessionID) // the state finalize always runs in
			if err := e.store.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
				t.Fatalf("SetMergeIntent: %v", err)
			}
			recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})

			e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true})

			intent, err := e.store.GetMergeIntent(context.Background(), chatID)
			if err != nil {
				t.Fatalf("GetMergeIntent: %v", err)
			}
			if intent == nil {
				t.Fatal("standing intent was cleared; #1142 requires it survive a head-modified merge failure")
			}

			req := fh.waitForDispatch(t, 2*time.Second)
			if req.Chat.LocalID != sessionID {
				t.Errorf("re-review dispatched for session %q, want %q", req.Chat.LocalID, sessionID)
			}
			// The task string is compiled into the envelope's <deliverable>;
			// every review variant of reviewDeliverableText opens "a review".
			if !strings.Contains(req.Ask.Message, "<deliverable>a review") {
				t.Errorf("re-review envelope is not the auto-review deliverable:\n%s", req.Ask.Message)
			}
			if req.Run.Setup == nil || req.Run.Setup.Repo != "https://github.com/acme/widgets.git" {
				t.Errorf("re-review dispatch missing the repo clone URL: %+v", req.Run.Setup)
			}
			if len(fh.calls()) != 1 {
				t.Errorf("dispatch calls = %d, want exactly 1", len(fh.calls()))
			}

			select {
			case body := <-posted:
				t.Fatalf("a comment was posted: %q; want none - the re-review is the outcome", body)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// An approval at the current head merges even when the pendingRun's own snapshot is empty
// (a resume whose re-fetch failed), with no "head moved" or re-review.
func TestTryMergeStandingIntentEmptySnapshotMergesOnMatchingHead(t *testing.T) {
	// mergeStub's fixed pull response reports head "headsha1" - the review's
	// commit_id matches it exactly, so the compare must see no movement.
	approved := `[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"},"commit_id":"headsha1"}]`
	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	srv := mergeStub(t, approved, "", posted, merged)
	defer srv.Close()

	e, fh := newTestExtension(t, srv.URL, []string{"merge"})
	sessionID := "github-acme-widgets-7"
	chatID := globalChatID(sessionID)
	pr := &pendingRun{
		sessionID: sessionID, owner: "acme", repo: "widgets", number: 7, login: autoReviewUser, isPR: true,
		gh:            githubContext{snap: Snapshot{IsPR: true, HeadSHA: "", BaseRef: "main"}},
		dispatched:    sdk.DispatchRequest{Run: sdk.RunConfig{Setup: &sdk.Setup{Repo: "https://github.com/acme/widgets.git"}}},
		defaultBranch: "main", installationID: 5,
	}
	e.pending.Store(chatID, pr)
	claimInflightFor(t, e, chatID, sessionID)
	if err := e.store.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatalf("SetMergeIntent: %v", err)
	}
	recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true})

	select {
	case <-merged:
	case <-time.After(2 * time.Second):
		t.Fatal("merge was not attempted; an unchanged head with an empty snapshot must still merge")
	}
	select {
	case c := <-posted:
		if strings.Contains(c, "head moved") {
			t.Errorf("comment = %q; the head did not move, must not claim it did", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no merge outcome appended")
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Errorf("dispatch calls = %d, want 0 - no re-review should have been dispatched", len(calls))
	}
	if intent, err := e.store.GetMergeIntent(context.Background(), chatID); err != nil || intent != nil {
		t.Errorf("intent = %+v, %v; want it cleared after the merge", intent, err)
	}
}

// Both synthetic builders produce the same shape, so a re-review is the label-trigger run.
func TestReReviewPayloadMatchesAutoReviewPayload(t *testing.T) {
	var p pullRequestPayload
	p.Action = "labeled"
	p.Number = 7
	p.PullRequest.Title = "Test PR"
	p.Repository.Name, p.Repository.Owner.Login = "widgets", "acme"
	p.Repository.CloneURL, p.Repository.DefaultBranch = "https://github.com/acme/widgets.git", "main"
	p.Installation.ID = 5
	want := autoReviewPayload(p, []byte(`{}`))
	got := reReviewPayload("acme", "widgets", 7, "Test PR", "https://github.com/acme/widgets.git", "main", 5)
	want.eventName, got.eventName = "", ""
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("reReviewPayload = %+v\nwant %+v", got, want)
	}
}

// mergeStub serves .../reviews (reviewsJSON), .../comments (commentsJSON, own-PR verdict markers);
// merged fires on PUT .../merge.
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
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/reviews/"),
			r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/comments/"):
			// The merge outcome lands as an edit of quack's own review/comment;
			// surfaced on the same channel so assertions read one stream.
			body, _ := io.ReadAll(r.Body)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			merged <- struct{}{}
			fmt.Fprint(w, `{"merged":true,"sha":"mergesha1"}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[{"id":1,"name":"go-test","status":"completed","conclusion":"success"}]}`)
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

// mergeStubDynamic serves reviews from a mutable value (setReviews) to simulate a review landing mid-dispatch;
// merged carries the PUT .../merge body for the head-sha guard.
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
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/reviews/"),
			r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/comments/"):
			// The merge outcome lands as an edit of quack's own review/comment;
			// surfaced on the same channel so assertions read one stream.
			body, _ := io.ReadAll(r.Body)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			body, _ := io.ReadAll(r.Body)
			merged <- string(body)
			fmt.Fprint(w, `{"merged":true,"sha":"mergesha1"}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[{"id":1,"name":"go-test","status":"completed","conclusion":"success"}]}`)
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

// mergePR's error wraps GitHub's raw 405 body: the line must name the failing check, never leak the JSON,
// and state what actually happens next.
func TestMergeFailureLineHumanizesRequiredCheckFailure(t *testing.T) {
	// A real 405 body GitHub returned.
	err := fmt.Errorf(`github: PUT /repos/acme/widgets/pulls/880/merge: status 405: {"message":"Required status check \"go-test\" is failing.","documentation_url":"https://docs.github.com/rest/pulls/pulls#merge-a-pull-request"}`)

	got := mergeFailureLine(err, "quack:fix")
	want := "Merge blocked: required check `go-test` is failing. Apply `quack:fix` or push a fix; re-review follows."
	if got != want {
		t.Errorf("mergeFailureLine(...) = %q, want %q", got, want)
	}
	if strings.Contains(got, "{") || strings.Contains(got, "documentation_url") {
		t.Errorf("humanized comment leaked the raw JSON body: %q", got)
	}
}

// Any error other than the required-check 405 collapses to one concise line without the raw JSON.
func TestMergeFailureLineConciseForOtherErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "other 405 message, JSON dropped",
			err:  fmt.Errorf(`github: PUT /repos/acme/widgets/pulls/7/merge: status 405: {"message":"Base branch was modified. Review and try the merge again.","documentation_url":"https://docs.github.com/x"}`),
			want: "Merge failed: Base branch was modified. Review and try the merge again.",
		},
		{
			name: "409 conflict, JSON dropped",
			err:  fmt.Errorf(`github: PUT /repos/acme/widgets/pulls/7/merge: status 409: {"message":"Head branch was modified. Review and try the merge again."}`),
			want: "Merge failed: Head branch was modified. Review and try the merge again.",
		},
		{
			name: "no JSON body at all - raw error text kept as-is",
			err:  fmt.Errorf("github: PUT %s: context deadline exceeded", "/repos/acme/widgets/pulls/7/merge"),
			want: "Merge failed: github: PUT /repos/acme/widgets/pulls/7/merge: context deadline exceeded.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeFailureLine(tt.err, "quack:fix"); got != tt.want {
				t.Errorf("mergeFailureLine(...) = %q, want %q", got, tt.want)
			}
		})
	}
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

// Merge-label cases decided without a dispatch: existing approval merges, non-approving verdict keeps the intent,
// trigger off or bot sender. No-review-yet is TestHandleWebhookMergeLabelDispatchesReview.
func TestHandleWebhookMergeLabel(t *testing.T) {
	approved := `[{"state":"CHANGES_REQUESTED","user":{"login":"quack[bot]"}},{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"}}]`
	tests := []struct {
		name        string
		triggers    []string
		reviews     string
		comments    string
		sender      string
		wantMerge   bool
		wantComment string
		wantIntent  bool
	}{
		{"approved review merges", []string{"merge"}, approved, "", "alice", true, "Merged", false},
		{"changes-requested stands by with the intent recorded", []string{"merge"},
			`[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"}},{"state":"CHANGES_REQUESTED","user":{"login":"quack[bot]"}}]`,
			"", "alice", false, "", true},
		{"COMMENTED carries no verdict but still stands by without a later approve", []string{"merge"},
			`[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"},{"state":"COMMENTED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-02T00:00:00Z"}]`,
			"", "alice", false, "", true},
		{"own-PR comment-review marker approves and merges", []string{"merge"}, `[]`,
			`[{"id":5,"user":{"login":"quack[bot]"},"body":"LGTM\n\n<!-- quack:delivery:review:approve -->","created_at":"2026-01-01T00:00:00Z"}]`,
			"alice", true, "Merged", false},
		{"own-PR comment-review marker request_changes stands by", []string{"merge"}, `[]`,
			`[{"id":5,"user":{"login":"quack[bot]"},"body":"needs work\n\n<!-- quack:delivery:review:request_changes -->","created_at":"2026-01-01T00:00:00Z"}]`,
			"alice", false, "", true},
		{"trigger not enabled is a no-op", []string{"mention"}, approved, "", "alice", false, "", false},
		{"bot sender cannot authorize", []string{"merge"}, approved, "", "other[bot]", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 2)
			merged := make(chan struct{}, 1)
			srv := mergeStub(t, tt.reviews, tt.comments, posted, merged)
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody(tt.sender)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantComment != "" {
				select {
				case c := <-posted:
					if !strings.Contains(c, tt.wantComment) || strings.Contains(c, "@") {
						t.Errorf("outcome = %q, want substring %q and no @-mention", c, tt.wantComment)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("expected an outcome")
				}
			} else {
				select {
				case c := <-posted:
					t.Errorf("nothing should be posted for a not-yet-mergeable PR; got %q", c)
				case <-time.After(100 * time.Millisecond):
				}
			}
			if tt.wantMerge {
				select {
				case <-merged:
				case <-time.After(2 * time.Second):
					t.Fatal("expected a merge PUT")
				}
			} else {
				select {
				case <-merged:
					t.Error("merge must not have been called")
				default:
				}
			}
			// None of these cases dispatches an orchestrator run - either the
			// verdict was already decided, or the trigger/sender gate refused first.
			select {
			case <-fh.notify:
				t.Error("this case must not dispatch an orchestrator run")
			default:
			}

			intent, err := ext.store.GetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"))
			if err != nil {
				t.Fatalf("GetMergeIntent: %v", err)
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

// quack:merge on an unreviewed PR records the intent AND dispatches a review, or the label does nothing.
// No "queued" comment: the reaction is the ack.
func TestHandleWebhookMergeLabelDispatchesReview(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	srv := mergeStub(t, "[]", "", posted, merged)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	select {
	case c := <-posted:
		t.Errorf("label posted %q; want no comment (the review is the outcome)", c)
	case <-time.After(100 * time.Millisecond):
	}
	if !strings.Contains(req.Ask.Message, "<deliverable>a review with inline comments and a verdict</deliverable>") {
		t.Errorf("dispatched envelope = %q; want the auto-review deliverable", req.Ask.Message)
	}

	intent, err := ext.store.GetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"))
	if err != nil || intent == nil || intent.RequestedBy != "alice" {
		t.Fatalf("GetMergeIntent = %+v, %v; want a recorded intent for alice", intent, err)
	}
}

// quack:merge while a review is in flight records the intent and waits, never a second concurrent review;
// RunEnded is never called, so the first run stays in flight.
func TestHandleWebhookMergeLabelWaitsForInFlightReview(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	srv := mergeStub(t, "[]", "", posted, merged)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"mention", "merge"})

	rec1 := httptest.NewRecorder()
	ext.handleWebhook(rec1, signedRequest("issue_comment", pullCommentBody("@quack review this")))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec1.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	rec2 := httptest.NewRecorder()
	ext.handleWebhook(rec2, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec2.Code)
	}

	select {
	case c := <-posted:
		t.Errorf("label posted %q; want no comment while the review runs", c)
	case <-time.After(100 * time.Millisecond):
	}

	intent, err := ext.store.GetMergeIntent(context.Background(), globalChatID("github-acme-widgets-7"))
	if err != nil || intent == nil {
		t.Fatalf("GetMergeIntent = %+v, %v; want a recorded intent", intent, err)
	}
	if calls := fh.calls(); len(calls) != 1 {
		t.Errorf("Host.Dispatch calls = %d; want 1 (the label must not dispatch a second review while one is in flight)", len(calls))
	}
}

// No review existed when quack:merge was applied; once the queued review lands approving,
// the PR merges on its own, naming the label-applier.
func TestHandleWebhookMergeLabelReviewLandsConsumesIntent(t *testing.T) {
	posted := make(chan string, 4)
	merged := make(chan string, 1)
	srv, setReviews := mergeStubDynamic(t, posted, merged)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"merge"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("alice")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	fh.waitForDispatch(t, 2*time.Second)

	// The review lands as an approval, then the review run's RunEnded records its delivery.
	setReviews(`[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"}]`)
	chatID := globalChatID("github-acme-widgets-7")
	recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "reviewed"})

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
		if !strings.Contains(c, "Merged as mergesh") || strings.Contains(c, "@") {
			t.Errorf("review edit = %q; want the merge sha appended and no @-mention", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no merge outcome appended to the review")
	}

	intent, err := ext.store.GetMergeIntent(context.Background(), chatID)
	if err != nil || intent != nil {
		t.Errorf("intent = %+v, %v; want it cleared after the merge", intent, err)
	}
}

// The standing intent survives a restart: a fresh Extension over the same store still honours it.
func TestHandleWebhookMergeLabelRestartSurvival(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	chatID := globalChatID("github-acme-widgets-7")
	if err := st.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatalf("seed merge intent: %v", err)
	}

	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	approved := `[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"}]`
	srv := mergeStub(t, approved, "", posted, merged)
	defer srv.Close()
	ext, fh := newTestExtensionWithStore(t, srv.URL, []string{"mention"}, st)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack review this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
	recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "reviewed"})

	select {
	case <-merged:
	case <-time.After(2 * time.Second):
		t.Fatal("the pre-restart standing intent did not merge once the review landed")
	}
	select {
	case c := <-posted:
		if !strings.Contains(c, "Merged as mergesh") || strings.Contains(c, "@") {
			t.Errorf("review edit = %q; want the merge sha appended and no @-mention", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no merge outcome appended to the review")
	}

	intent, err := st.GetMergeIntent(context.Background(), chatID)
	if err != nil || intent != nil {
		t.Errorf("intent = %+v, %v; want it cleared after the merge", intent, err)
	}
}

// e.pending is in-memory, so RunEnded on a SECOND Extension over the same store (a restart)
// must still finalize and merge off the durable pendingRun row.
func TestRunEndedAfterRestartStillFinalizesAndMerges(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	chatID := globalChatID("github-acme-widgets-7")
	if err := st.SetMergeIntent(context.Background(), chatID, "alice"); err != nil {
		t.Fatalf("seed merge intent: %v", err)
	}

	posted := make(chan string, 4)
	merged := make(chan struct{}, 1)
	approved := `[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"},"submitted_at":"2026-01-01T00:00:00Z"}]`
	srv := mergeStub(t, approved, "", posted, merged)
	defer srv.Close()

	ext, fh := newTestExtensionWithStore(t, srv.URL, []string{"mention"}, st)
	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack review this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	if row, err := st.GetPendingRun(context.Background(), chatID); err != nil || row == nil {
		t.Fatalf("GetPendingRun = %+v, %v; want the dispatch to have persisted a row", row, err)
	}

	// Simulate a quack restart: a brand new Extension, with an empty
	// e.pending, over the same durable store.
	restarted, _ := newTestExtensionWithStore(t, srv.URL, []string{"mention"}, st)
	recordDelivery(chatID, deliveryOutcome{reviewDelivered: true})
	restarted.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "reviewed"})

	select {
	case <-merged:
	case <-time.After(2 * time.Second):
		t.Fatal("RunEnded on the restarted Extension did not merge on approval")
	}
	select {
	case c := <-posted:
		if !strings.Contains(c, "Merged as mergesh") || strings.Contains(c, "@") {
			t.Errorf("review edit = %q; want the merge sha appended and no @-mention", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no merge outcome appended to the review")
	}

	if intent, err := st.GetMergeIntent(context.Background(), chatID); err != nil || intent != nil {
		t.Errorf("intent = %+v, %v; want it cleared after the merge", intent, err)
	}
	if row, err := st.GetPendingRun(context.Background(), chatID); err != nil || row != nil {
		t.Errorf("pending run row = %+v, %v; want it deleted by finalize", row, err)
	}
}

// A RunEnded with neither an in-memory entry nor a durable row must log-and-drop, not merge or panic.
func TestRunEndedWithNoPendingDispatchDropsOutcome(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	chatID := globalChatID("github-acme-widgets-7")

	ext, _ := newTestExtensionWithStore(t, "http://127.0.0.1:0", []string{"mention"}, st)
	// No dispatch happened: e.pending is empty and no row exists, so
	// RunEnded must take the original dropping-the-outcome branch.
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "done"})

	if row, err := st.GetPendingRun(context.Background(), chatID); err != nil || row != nil {
		t.Errorf("row = %+v, %v; want none", row, err)
	}
}

// A Dispatch failure must delete the row SetPendingRun just wrote, like finalize's success-path defers.
func TestDispatchFailureDeletesPendingRunRow(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	approved := `[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"}}]`
	posted := make(chan string, 2)
	srv := mergeStub(t, approved, "", posted, make(chan struct{}, 1))
	defer srv.Close()

	ext, fh := newTestExtensionWithStore(t, srv.URL, []string{"mention"}, st)
	fh.dispatchErr = errors.New("dispatch failed")
	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", pullCommentBody("@quack review this")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}

	chatID := globalChatID("github-acme-widgets-7")
	deadline := time.After(2 * time.Second)
	for {
		row, err := st.GetPendingRun(context.Background(), chatID)
		if err != nil {
			t.Fatalf("GetPendingRun: %v", err)
		}
		if row == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("pending run row survived a failed dispatch, want it deleted")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A sender outside allowed_users can never authorize a merge, even with an APPROVED review on the PR.
func TestHandleWebhookMergeLabelRespectsAllowlist(t *testing.T) {
	approved := `[{"id":11,"state":"APPROVED","user":{"login":"quack[bot]"}}]`
	posted := make(chan string, 2)
	merged := make(chan struct{}, 1)
	srv := mergeStub(t, approved, "", posted, merged)
	defer srv.Close()
	ext, _ := newTestExtension(t, srv.URL, []string{"merge"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", mergeLabelBody("mallory")))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	select {
	case <-merged:
		t.Error("merge-label sender not in allowed_users must not authorize a merge")
	default:
	}
}

// A label trigger implies work, so a proxy "delivered" signal must never suppress a plan-only run's
// summary comment - that comment IS the deliverable.
func TestHandleWebhookPlanLabelPostsPlanEvenWhenDelivered(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", "quack:plan", "alice", false)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)

	chatID := globalChatID("github-acme-widgets-7")
	// No takeDeliveryDetail entry recorded - a plan-only run never delivers
	// anything staged, so finalize must fall through to posting the answer.
	ext.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "## Plan\n\nthe plan"})

	select {
	case body := <-posted:
		if !strings.Contains(body, "the plan") {
			t.Errorf("posted comment is not the plan: %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("plan-only run did not post its plan - the delivered-skip dropped it")
	}
}

// A label-triggered run reacts on the ISSUE (/issues/{number}/reactions): a label event has no comment ID.
func TestHandleWebhookLabelPostsEyesReactionOnIssue(t *testing.T) {
	for _, tc := range []struct{ trigger, label string }{
		{"issue_plan", "quack:plan"},
		{"issue_implement", "quack:implement"},
	} {
		t.Run(tc.trigger, func(t *testing.T) {
			reacted := make(chan string, 1)
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

			ext, _ := newTestExtension(t, srv.URL, []string{tc.trigger})

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

// TestHandleWebhookIssuePlanLabel pins the quack:plan label routing and the
// dispatched plan message's framing.
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
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", tt.label, tt.sender, tt.isPR)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if !tt.wantRun {
				select {
				case <-fh.notify:
					t.Error("issues event should not have dispatched a run")
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			req := fh.waitForDispatch(t, 2*time.Second)
			msg := req.Ask.Message
			// Title/body come from the fetched snapshot (stubGitHub's issue meta), not the raw webhook payload.
			if !strings.Contains(msg, "Test issue") {
				t.Errorf("plan message missing issue context: %q", msg)
			}
			if !strings.Contains(msg, "PLANNING-ONLY") {
				t.Errorf("plan message not framed planning-only: %q", msg)
			}
			// The plan-only prompt must say the answer text IS the deliverable, not a pointer to a discarded file.
			if !strings.Contains(msg, "ANSWER TEXT is the plan") {
				t.Errorf("plan message does not state the answer text is the deliverable: %q", msg)
			}
			// File-path and stale-version cautions live in the orchestrator prompt, not the trigger.
			for _, moved := range []string{"discarded", "current stable"} {
				if strings.Contains(msg, moved) {
					t.Errorf("plan message still carries the %q caution - it should have moved to the orchestrator bundle prompt: %q", moved, msg)
				}
			}
			for _, banned := range []string{"git_push", "github_pull_request", "create a branch"} {
				if strings.Contains(msg, banned) {
					t.Errorf("planning-only message contains delivery instruction %q: %q", banned, msg)
				}
			}
			if req.Chat.LocalID != "github-acme-widgets-7" {
				t.Errorf("LocalID = %q, want issue-tied github-acme-widgets-7", req.Chat.LocalID)
			}
		})
	}
}

// TestHandleWebhookIssueOpenedNoOp pins that non-labeled issue actions are
// ignored - the workflow is label-driven, not event-driven.
func TestHandleWebhookIssueOpenedNoOp(t *testing.T) {
	ext, fh := newTestExtension(t, "http://unused", []string{"issue_plan"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issues", issuesBody("opened", "", "alice", false)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 no-op ack", rec.Code)
	}
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("issues opened should not dispatch a run")
	}
}

// TestHandleWebhookIssueImplementLabel pins the quack:implement label routing
// and its dispatched task's Closes-trailer framing.
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
			srv := stubGitHub(t, make(chan string, 4))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issues", issuesBody("labeled", tt.label, "alice", false)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if !tt.wantRun {
				select {
				case <-fh.notify:
					t.Error("issues event should not have dispatched a run")
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			req := fh.waitForDispatch(t, 2*time.Second)
			for _, want := range []string{"Closes #7", `<issue number="7">`} {
				if !strings.Contains(req.Ask.Message, want) {
					t.Errorf("implement message missing %q: %q", want, req.Ask.Message)
				}
			}
			if req.Chat.LocalID != "github-acme-widgets-7" {
				t.Errorf("LocalID = %q, want the issue's session (plan continuity)", req.Chat.LocalID)
			}
		})
	}
}

// A request_changes review on a PR quack authored engages it; authorship is the flag, no label needed.
func TestHandleWebhookRequestChangesEngagesOwnPR(t *testing.T) {
	posted := make(chan string, 4)
	srv := stubFixGitHubFull(t, posted, nil, false, "", "quack[bot]") // no labels; PR authored by quack itself
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request_review", pullRequestReviewBody("changes_requested", "alice", 7)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	req := fh.waitForDispatch(t, 2*time.Second)
	for _, want := range []string{"requested changes", `"actor":"alice"`} {
		if !strings.Contains(req.Ask.Message, want) {
			t.Errorf("engagement message missing %q: %q", want, req.Ask.Message)
		}
	}
}

// Label/mention triggers own PRs quack did not author; approving/commented reviews never engage.
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
			srv := stubFixGitHubFull(t, posted, nil, false, "", tt.prAuthorLogin)
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("pull_request_review", pullRequestReviewBody(tt.state, "alice", 7)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			// Authorship resolves async (an HTTP round trip); bound the wait and
			// fail immediately if it fires.
			select {
			case <-fh.notify:
				t.Error("must not engage")
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

// reviewCommandBody is an issue_comment payload on a PR, with labels and author association.
func reviewCommandBody(comment, association string, labels ...string) []byte {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = fmt.Sprintf(`{"name":%q}`, l)
	}
	return []byte(fmt.Sprintf(`{
		"action":"created",
		"comment":{"id":999,"body":%q,"user":{"login":"alice"},"author_association":%q},
		"issue":{"number":7,"labels":[%s],"pull_request":{}},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, comment, association, strings.Join(quoted, ",")))
}

func TestHandleWebhookReviewCommand(t *testing.T) {
	tests := []struct {
		name     string
		triggers []string
		body     []byte
		wantRun  bool
	}{
		{"bare /review with label and write access fires", []string{"label"}, reviewCommandBody("  /review \n", "COLLABORATOR", "quack-auto-review"), true},
		{"owner association fires", []string{"label"}, reviewCommandBody("/review", "OWNER", "quack-auto-review"), true},
		{"missing review label is a no-op", []string{"label"}, reviewCommandBody("/review", "OWNER", "other"), false},
		{"read-only author is a no-op", []string{"label"}, reviewCommandBody("/review", "CONTRIBUTOR", "quack-auto-review"), false},
		{"extra text is not the command", []string{"label"}, reviewCommandBody("/review please", "OWNER", "quack-auto-review"), false},
		{"label trigger disabled is a no-op", []string{"mention"}, reviewCommandBody("/review", "OWNER", "quack-auto-review"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("issue_comment", tt.body))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				req := fh.waitForDispatch(t, 2*time.Second)
				if !strings.Contains(req.Ask.Message, "<deliverable>a review with inline comments and a verdict</deliverable>") {
					t.Errorf("ask = %q; want the auto-review deliverable", req.Ask.Message)
				}
			} else {
				select {
				case <-fh.notify:
					t.Errorf("%s: should not have dispatched a run", tt.name)
				case <-time.After(200 * time.Millisecond):
				}
			}
		})
	}
}

// TestHandleWebhookReviewCommandRespectsAllowlist pins that /review still goes
// through the allowed_users gate like every human-invoked trigger.
func TestHandleWebhookReviewCommandRespectsAllowlist(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"label"})
	ext.allowedUsers = map[string]bool{"someone-else": true}

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", reviewCommandBody("/review", "OWNER", "quack-auto-review")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	select {
	case <-fh.notify:
		t.Error("disallowed user should not dispatch")
	case <-time.After(200 * time.Millisecond):
	}
}

// A /review during an in-flight review: the dedup ack lands on the SAME comment handleIssueComment already
// reacted to, never also the issue (different targets would both stay visible).
func TestReviewCommandWhileRunningReactsOnOneTargetOnly(t *testing.T) {
	var commentReactions, issueReactions int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.Contains(r.URL.Path, "/comments/") && strings.HasSuffix(r.URL.Path, "/reactions"):
			atomic.AddInt32(&commentReactions, 1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			atomic.AddInt32(&issueReactions, 1)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	ext, _ := newTestExtension(t, srv.URL, []string{"label"})

	// A review is already running for this session - claimInflight will
	// reject the incoming /review as a duplicate.
	sessionID := "github-acme-widgets-7"
	ext.inflight.Store(sessionID, time.Now())

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", reviewCommandBody("/review", "OWNER", "quack-auto-review")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	time.Sleep(300 * time.Millisecond)

	if atomic.LoadInt32(&commentReactions) == 0 {
		t.Error("no reaction landed on the triggering comment")
	}
	if got := atomic.LoadInt32(&issueReactions); got != 0 {
		t.Errorf("issue reactions = %d; want 0 - the comment already carries the ack, a second reaction on the issue is a second visible reaction for one trigger", got)
	}
}

// synchronize invalidates the clone only for a PR with a run in flight; with none there is nothing to refresh.
func TestHandleWebhookSynchronizeInvalidatesOnlyARunningPR(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, nil)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("synchronize", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := fh.invalidateCalls(); len(got) != 0 {
		t.Fatalf("InvalidateSetup calls with no run in flight = %v, want none", got)
	}

	chatID := globalChatID("github-acme-widgets-7")
	ext.pending.Store(chatID, &pendingRun{sessionID: "github-acme-widgets-7", owner: "acme", repo: "widgets", number: 7, isPR: true})

	rec = httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("synchronize", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := fh.invalidateCalls(); len(got) != 1 || got[0] != chatID {
		t.Fatalf("InvalidateSetup calls = %v, want [%s]", got, chatID)
	}

	select {
	case <-fh.notify:
		t.Error("pull_request.synchronize should never dispatch a run")
	case <-time.After(100 * time.Millisecond):
	}
}

// stubGitHubPullSequence serves a caller-controlled sequence of /pulls/<n> responses:
// first the snapshot fetch, then the blank-head-ref refetch.
func stubGitHubPullSequence(t *testing.T, comments chan string, pulls ...string) *httptest.Server {
	t.Helper()
	var n atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/app"):
			fmt.Fprint(w, `{"slug":"quack"}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			comments <- string(body)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			i := int(n.Add(1)) - 1
			if i >= len(pulls) {
				i = len(pulls) - 1 // retries past the configured sequence repeat the last response
			}
			resp := pulls[i]
			if resp == "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, resp)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

// prPayload is issueCommentPayloadFor with Issue.PullRequest set, so
// dispatch() takes the isPR branch.
func prPayload(owner, repo string, number int, login, body string) issueCommentPayload {
	p := issueCommentPayloadFor(owner, repo, number, login, body, "")
	p.Issue.PullRequest = &struct{}{}
	return p
}

const prMetaWithHead = `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"}}`
const prMetaNoHead = `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"","sha":""},"base":{"ref":"main"}}`

// dispatch() must never send a PR Setup with a blank ExistingHeadRef.
func TestDispatchPRHeadRef(t *testing.T) {
	// /pulls/ sequence: snapshot pullMeta, authoredByQuack's prAuthor, then the head-ref refetch only if blank.

	t.Run("snapshot has ref: passthrough, no refetch", func(t *testing.T) {
		srv := stubGitHubPullSequence(t, make(chan string, 8), prMetaWithHead, prMetaWithHead)
		defer srv.Close()
		e, fh := newTestExtension(t, srv.URL, nil)

		e.dispatch(prPayload("acme", "widgets", 7, "alice", "/review"), "review")

		calls := fh.calls()
		if len(calls) != 1 {
			t.Fatalf("Dispatch calls = %d, want 1", len(calls))
		}
		if got := calls[0].Run.Setup.ExistingHeadRef; got != "feature-branch" {
			t.Errorf("ExistingHeadRef = %q, want feature-branch", got)
		}
	})

	t.Run("snapshot missing ref but API refetch succeeds", func(t *testing.T) {
		srv := stubGitHubPullSequence(t, make(chan string, 8), prMetaNoHead, prMetaNoHead, prMetaWithHead)
		defer srv.Close()
		e, fh := newTestExtension(t, srv.URL, nil)

		e.dispatch(prPayload("acme", "widgets", 7, "alice", "/review"), "review")

		calls := fh.calls()
		if len(calls) != 1 {
			t.Fatalf("Dispatch calls = %d, want 1", len(calls))
		}
		if got := calls[0].Run.Setup.ExistingHeadRef; got != "feature-branch" {
			t.Errorf("ExistingHeadRef = %q, want feature-branch (fixed via refetch)", got)
		}
	})

	t.Run("snapshot missing ref and API refetch fails: abort, no dispatch", func(t *testing.T) {
		postedComments := make(chan string, 8)
		srv := stubGitHubPullSequence(t, postedComments, prMetaNoHead, prMetaNoHead, "")
		defer srv.Close()
		e, fh := newTestExtension(t, srv.URL, nil)

		e.dispatch(prPayload("acme", "widgets", 7, "alice", "/review"), "review")

		if calls := fh.calls(); len(calls) != 0 {
			t.Fatalf("Dispatch calls = %d, want 0 (must not dispatch with a blank head ref)", len(calls))
		}
		select {
		case body := <-postedComments:
			if !strings.Contains(body, "head branch") {
				t.Errorf("abort comment = %q, missing explanation", body)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no abort comment posted")
		}
	})
}

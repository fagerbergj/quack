package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/store"
)

// stubFixGitHub is stubGitHub plus the checks API (commits/{sha}/check-runs,
// check-runs/{id}/annotations), configurable PR labels, and a configurable PR
// author - what the #254/#656 auto-heal + authorship paths read. failing
// toggles whether the head commit has a failed check run; commitAuthorEmail
// is the /commits/{sha} author email the ONE-attempt guard reads (default
// "" - a human commit); prAuthorLogin is the /pulls/{n} author login (default
// "someone-else" - not quack).
func stubFixGitHub(t *testing.T, posted chan<- string, prLabels []string, failing bool) *httptest.Server {
	t.Helper()
	return stubFixGitHubFull(t, posted, prLabels, failing, "", "someone-else")
}

func stubFixGitHubFull(t *testing.T, posted chan<- string, prLabels []string, failing bool, commitAuthorEmail, prAuthorLogin string) *httptest.Server {
	t.Helper()
	labelsJSON := make([]string, 0, len(prLabels))
	for _, l := range prLabels {
		labelsJSON = append(labelsJSON, fmt.Sprintf(`{"name":%q}`, l))
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
		case strings.HasSuffix(r.URL.Path, "/check-runs") && failing:
			fmt.Fprint(w, `{"check_runs":[{"id":42,"name":"go-test","conclusion":"failure","html_url":"https://ci/42","output":{"title":"tests failed","summary":"1 test failed"}},{"id":43,"name":"lint","conclusion":"success"}]}`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, `{"check_runs":[{"id":43,"name":"lint","conclusion":"success"}]}`)
		case strings.HasSuffix(r.URL.Path, "/annotations"):
			fmt.Fprint(w, `[{"path":"internal/foo.go","start_line":12,"annotation_level":"failure","message":"TestFoo failed: want 2, got 3"}]`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/commits/"):
			// commitAuthorEmail (the one-attempt guard) - matched before the bare
			// "/commits" list below.
			fmt.Fprintf(w, `{"commit":{"author":{"email":%q}}}`, commitAuthorEmail)
		case strings.HasSuffix(r.URL.Path, "/commits"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
			posted <- string(body)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprintf(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"},"user":{"login":%q}}`, prAuthorLogin)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprintf(w, `{"title":"Test PR","body":"A test PR.","state":"open","labels":[%s]}`, strings.Join(labelsJSON, ","))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func newFixExtension(t *testing.T, runner Runner, apiBase string, st *store.Store, triggers ...string) *Extension {
	t.Helper()
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = apiBase
	return NewExtension(app, config.GitHubExtensionConfig{
		WebhookSecret: testSecret,
		Mention:       "@quack", // this file doesn't exercise mention matching; keep the literal "@quack review this" bodies elsewhere valid
		Triggers:      triggers,
		AllowedUsers:  []string{"alice"},
	}, runner, st, nil)
}

func workflowRunBody(action, conclusion, sha string, prNumbers ...int) []byte {
	prs := make([]string, 0, len(prNumbers))
	for _, n := range prNumbers {
		prs = append(prs, fmt.Sprintf(`{"number":%d}`, n))
	}
	return []byte(fmt.Sprintf(`{
		"action":%q,
		"workflow_run":{"name":"CI","head_sha":%q,"conclusion":%q,"html_url":"https://ci/run/1","pull_requests":[%s]},
		"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
		"installation":{"id":5}
	}`, action, sha, conclusion, strings.Join(prs, ",")))
}

func newFixTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

// Eligibility (#656): a failing workflow_run only dispatches a fix run when
// the PR carries quack:fix OR quack itself authored the PR - either is
// sufficient, neither requires the label to have just been (re-)applied.
func TestWorkflowRunAutoHealEligibility(t *testing.T) {
	tests := []struct {
		name          string
		triggers      []string
		prLabels      []string
		prAuthorLogin string
		wantRun       bool
	}{
		{"fix label + ci_fix trigger fires", []string{"ci_fix"}, []string{"quack:fix"}, "someone-else", true},
		{"no label, not quack's PR never heals", []string{"ci_fix"}, []string{"enhancement"}, "someone-else", false},
		{"no label, but quack authored the PR heals (authorship is the flag)", []string{"ci_fix"}, nil, "quack[bot]", true},
		{"trigger not enabled is a no-op", []string{"mention"}, []string{"quack:fix"}, "someone-else", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 4)
			gh := stubFixGitHubFull(t, posted, tt.prLabels, true, "", tt.prAuthorLogin)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
			ext := newFixExtension(t, runner, gh.URL, newFixTestStore(t), tt.triggers...)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha1", 7)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				select {
				case msg := <-runner.gotMessage:
					for _, want := range []string{`<pull_request number="7">`, "commits on this PR's head branch that make the failing checks pass", `"conclusion":"failure"`} {
						if !strings.Contains(msg, want) {
							t.Errorf("fix run message missing %q: %q", want, msg)
						}
					}
				case <-time.After(2 * time.Second):
					t.Fatal("auto-heal did not dispatch a fix run")
				}
			} else {
				time.Sleep(150 * time.Millisecond)
				if atomic.LoadInt32(&runner.calls) != 0 {
					t.Error("auto-heal must not dispatch when ineligible")
				}
			}
		})
	}
}

// Non-failure conclusions and non-completed actions never dispatch, and a nil
// store disables auto-heal outright (the retry bound must be durable).
func TestWorkflowRunIgnored(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		nilSt   bool
		wantAck int
	}{
		{"success conclusion", workflowRunBody("completed", "success", "sha1", 7), false, http.StatusOK},
		{"requested action", workflowRunBody("requested", "", "sha1", 7), false, http.StatusOK},
		{"no PR mapping (fork or bare push)", workflowRunBody("completed", "failure", "sha1"), false, http.StatusOK},
		{"nil store refuses an unboundable loop", workflowRunBody("completed", "failure", "sha1", 7), true, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posted := make(chan string, 4)
			gh := stubFixGitHub(t, posted, []string{"quack:fix"}, true)
			defer gh.Close()

			runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
			var st *store.Store
			if !tt.nilSt {
				st = newFixTestStore(t)
			}
			ext := newFixExtension(t, runner, gh.URL, st, "ci_fix")

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("workflow_run", tt.body))
			if rec.Code != tt.wantAck {
				t.Fatalf("status = %d; want %d", rec.Code, tt.wantAck)
			}
			time.Sleep(100 * time.Millisecond)
			if atomic.LoadInt32(&runner.calls) != 0 {
				t.Error("no fix run should dispatch")
			}
		})
	}
}

// Loop prevention: several failing workflows on ONE head commit (CI usually
// runs a few) dispatch exactly one fix run.
func TestWorkflowRunSameSHADeduped(t *testing.T) {
	posted := make(chan string, 8)
	gh := stubFixGitHub(t, posted, []string{"quack:fix"}, true)
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 2), answer: "fixed"}
	st := newFixTestStore(t)
	ext := newFixExtension(t, runner, gh.URL, st, "ci_fix")

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha1", 7)))
		select {
		case <-runner.gotMessage:
			if i == 1 {
				t.Fatal("second failure on the same head commit must not dispatch again")
			}
		case <-time.After(2 * time.Second):
			if i == 0 {
				t.Fatal("first failure did not dispatch")
			}
		}
	}

	fs, err := st.GetGithubFixState(context.Background(), "github-acme-widgets-7")
	if err != nil || fs == nil {
		t.Fatalf("fix state = %v, %v; want a persisted row", fs, err)
	}
	if fs.LastSHA != "sha1" || fs.Stopped {
		t.Errorf("fix state = %+v; want LastSHA=sha1 Stopped=false", fs)
	}
}

// The Forbidden section's ONE rule: if quack's OWN fix push also fails CI, it
// must NOT fix again - it stops and comments why, and the state survives a
// process restart. A LATER failure caused by a NEW (human) commit heals again
// with no human action required - the guard is keyed on the failing commit's
// actual author, not a counter that needs resetting.
func TestAutoHealOneAttemptGuard(t *testing.T) {
	posted := make(chan string, 4)
	// commitAuthorEmail "agent@quack.local" - the failing commit IS quack's own.
	gh := stubFixGitHubFull(t, posted, []string{"quack:fix"}, true, "agent@quack.local", "someone-else")
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
	st := newFixTestStore(t)
	// Seed the state a REAL prior failure/fix cycle would have left: sha1 (a
	// human commit) already failed once and quack already dispatched a fix for
	// it - sha2 below is that fix's own CI run failing.
	sessionID := "github-acme-widgets-7"
	if err := st.SetGithubFixState(context.Background(), store.GithubFixState{ChatID: sessionID, LastSHA: "sha1"}); err != nil {
		t.Fatalf("seed fix state: %v", err)
	}
	ext := newFixExtension(t, runner, gh.URL, st, "ci_fix")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha2", 7)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case c := <-posted:
		for _, want := range []string{"Auto-heal stopped", "won't attempt a second fix"} {
			if !strings.Contains(c, want) {
				t.Errorf("stop comment missing %q: %q", want, c)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no stop comment posted")
	}
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&runner.calls) != 0 {
		t.Error("must not dispatch a second fix for its own failing commit")
	}

	fs, err := st.GetGithubFixState(context.Background(), "github-acme-widgets-7")
	if err != nil || fs == nil || !fs.Stopped || fs.LastSHA != "sha2" {
		t.Fatalf("fix state = %+v, %v; want Stopped=true LastSHA=sha2", fs, err)
	}

	// "Restart": a fresh Extension over the same store. A sibling workflow
	// failing on the SAME commit stays silent (dedup, not a second stop comment).
	runner2 := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
	ext2 := newFixExtension(t, runner2, gh.URL, st, "ci_fix")
	rec2 := httptest.NewRecorder()
	ext2.handleWebhook(rec2, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha2", 7)))
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&runner2.calls) != 0 {
		t.Error("stopped state must survive a restart; no run may dispatch")
	}
	select {
	case c := <-posted:
		t.Errorf("stopped auto-heal posted a second comment for the same commit: %q", c)
	default:
	}

	// A NEW commit (a human's, not quack's) fails - auto-heal resumes with no
	// relabeling and no counter to reset.
	gh3 := stubFixGitHubFull(t, posted, []string{"quack:fix"}, true, "", "someone-else")
	defer gh3.Close()
	runner3 := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
	ext3 := newFixExtension(t, runner3, gh3.URL, st, "ci_fix")
	rec3 := httptest.NewRecorder()
	ext3.handleWebhook(rec3, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha3", 7)))
	select {
	case <-runner3.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("a NEW human-authored failure should heal again with no re-labelling")
	}
}

// On a PR quack itself authored, EVERY commit is quack's, including the very
// first one it opened the PR with - the FIRST-ever CI failure must still get
// a fix attempt, not read as "my own fix already failed" (see autoHeal's
// st != nil gate on the one-attempt guard).
func TestAutoHealAuthoredPRFirstFailureGetsAFix(t *testing.T) {
	posted := make(chan string, 4)
	gh := stubFixGitHubFull(t, posted, nil, true, "agent@quack.local", "quack[bot]")
	defer gh.Close()

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
	ext := newFixExtension(t, runner, gh.URL, newFixTestStore(t), "ci_fix")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha1", 7)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	select {
	case <-runner.gotMessage:
	case <-time.After(2 * time.Second):
		t.Fatal("the first CI failure on a PR quack authored itself must still get a fix attempt")
	}
}

// Re-applying quack:fix is the retry convention: it re-arms auto-heal (clears
// a prior stop) and, since CI is still failing, fixes it immediately - no
// waiting for the next CI event.
func TestFixLabelReapplyRearms(t *testing.T) {
	posted := make(chan string, 4)
	gh := stubFixGitHub(t, posted, []string{"quack:fix"}, true)
	defer gh.Close()

	st := newFixTestStore(t)
	sessionID := "github-acme-widgets-7"
	if err := st.SetGithubFixState(context.Background(), store.GithubFixState{ChatID: sessionID, LastSHA: "headsha1", Stopped: true}); err != nil {
		t.Fatalf("seed fix state: %v", err)
	}

	runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
	ext := newFixExtension(t, runner, gh.URL, st, "ci_fix")

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("labeled", "quack:fix")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case msg := <-runner.gotMessage:
		if !strings.Contains(msg, "commits on this PR's head branch that make the failing checks pass") {
			t.Errorf("re-armed run message = %q; want the fix deliverable", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("re-armed auto-heal did not dispatch")
	}

	fs, err := st.GetGithubFixState(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetGithubFixState: %v", err)
	}
	if fs == nil || fs.Stopped {
		t.Errorf("fix state = %+v; want the prior Stopped=true cleared", fs)
	}
}

// The label event's other half: an allowlisted human applying quack:fix to a
// PR with nothing currently failing does NOTHING observable - no phantom
// review, no comment - the flag just arms silently for the next CI failure
// (#655). A non-allowlisted sender is refused outright.
func TestFixLabelApplied(t *testing.T) {
	fixLabelBody := func(sender string) []byte {
		return []byte(fmt.Sprintf(`{
			"action":"labeled",
			"number":7,
			"pull_request":{"title":"Test PR","head":{"sha":"headsha1"}},
			"label":{"name":"quack:fix"},
			"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
			"installation":{"id":5},
			"sender":{"login":%q}
		}`, sender))
	}

	t.Run("failing checks dispatch a fix run", func(t *testing.T) {
		posted := make(chan string, 4)
		gh := stubFixGitHub(t, posted, nil, true)
		defer gh.Close()
		runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
		ext := newFixExtension(t, runner, gh.URL, newFixTestStore(t), "ci_fix")

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("pull_request", fixLabelBody("alice")))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
		select {
		case msg := <-runner.gotMessage:
			for _, want := range []string{`<pull_request number="7">`, "commits on this PR's head branch that make the failing checks pass", `"name":"quack:fix"`, `"login":"alice"`} {
				if !strings.Contains(msg, want) {
					t.Errorf("fix run message missing %q: %q", want, msg)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("fix label did not dispatch a run")
		}
	})

	t.Run("nothing failing arms the flag silently, no phantom review", func(t *testing.T) {
		posted := make(chan string, 4)
		gh := stubFixGitHub(t, posted, nil, false)
		defer gh.Close()
		runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
		ext := newFixExtension(t, runner, gh.URL, newFixTestStore(t), "ci_fix")

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("pull_request", fixLabelBody("alice")))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
		time.Sleep(150 * time.Millisecond)
		if atomic.LoadInt32(&runner.calls) != 0 {
			t.Error("no run should dispatch when nothing is failing (#655)")
		}
		select {
		case c := <-posted:
			t.Errorf("no comment should be posted on a green PR: %q", c)
		default:
		}
	})

	t.Run("non-allowlisted sender is refused", func(t *testing.T) {
		posted := make(chan string, 4)
		gh := stubFixGitHub(t, posted, nil, true)
		defer gh.Close()
		runner := &fakeRunner{gotMessage: make(chan string, 1), answer: "fixed"}
		ext := newFixExtension(t, runner, gh.URL, newFixTestStore(t), "ci_fix")

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("pull_request", fixLabelBody("mallory")))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 no-op", rec.Code)
		}
		time.Sleep(100 * time.Millisecond)
		if atomic.LoadInt32(&runner.calls) != 0 {
			t.Error("non-allowlisted sender must not dispatch")
		}
	})
}

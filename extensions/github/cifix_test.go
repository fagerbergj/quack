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
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// stubFixGitHub is stubFixGitHubFull with a default PR author ("someone-else"
// - not quack) and no commit-author override.
func stubFixGitHub(t *testing.T, posted chan<- string, prLabels []string, failing bool) *httptest.Server {
	t.Helper()
	return stubFixGitHubFull(t, posted, prLabels, failing, "", "someone-else")
}

// stubFixGitHubFull is stubGitHub plus the checks API, PR labels and PR author. commitAuthorEmail
// defaults to "" (a human commit); prAuthorLogin defaults to "someone-else" (not quack).
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

// A failing workflow_run dispatches a fix only when the PR carries quack:fix OR quack authored it;
// either suffices, and the label need not have just been (re-)applied.
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
			srv := stubFixGitHubFull(t, posted, tt.prLabels, true, "", tt.prAuthorLogin)
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha1", 7)))
			if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}

			if tt.wantRun {
				req := fh.waitForDispatch(t, 2*time.Second)
				for _, want := range []string{`<pull_request number="7">`, "commits on this PR's head branch that make the failing checks pass", "go-test"} {
					if !strings.Contains(req.Ask.Message, want) {
						t.Errorf("fix run message missing %q: %q", want, req.Ask.Message)
					}
				}
			} else {
				select {
				case <-fh.notify:
					t.Error("auto-heal must not dispatch when ineligible")
				case <-time.After(150 * time.Millisecond):
				}
			}
		})
	}
}

// Non-failure conclusions and non-completed actions never dispatch. The original's
// nil-store case does not port: factory always sets a non-nil store (github/store.go's newStore).
func TestWorkflowRunIgnored(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"success conclusion", workflowRunBody("completed", "success", "sha1", 7)},
		{"requested action", workflowRunBody("requested", "", "sha1", 7)},
		{"no PR mapping (fork or bare push)", workflowRunBody("completed", "failure", "sha1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubFixGitHub(t, make(chan string, 4), []string{"quack:fix"}, true)
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

			rec := httptest.NewRecorder()
			ext.handleWebhook(rec, signedRequest("workflow_run", tt.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200", rec.Code)
			}
			// Filtered in handleWorkflowRun before autoHeal is ever spawned.
			if calls := fh.calls(); len(calls) != 0 {
				t.Error("no fix run should dispatch")
			}
		})
	}
}

// Loop prevention: several failing workflows on ONE head commit (CI usually
// runs a few) dispatch exactly one fix run.
func TestWorkflowRunSameSHADeduped(t *testing.T) {
	srv := stubFixGitHub(t, make(chan string, 8), []string{"quack:fix"}, true)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha1", 7)))
		if i == 0 {
			fh.waitForDispatch(t, 2*time.Second)
		} else {
			select {
			case <-fh.notify:
				t.Fatal("second failure on the same head commit must not dispatch again")
			case <-time.After(300 * time.Millisecond):
			}
		}
	}

	fs, err := ext.store.GetFixState(context.Background(), globalChatID("github-acme-widgets-7"))
	if err != nil || fs == nil {
		t.Fatalf("fix state = %v, %v; want a persisted row", fs, err)
	}
	if fs.LastSHA != "sha1" || fs.Stopped {
		t.Errorf("fix state = %+v; want LastSHA=sha1 Stopped=false", fs)
	}
}

// If quack's own fix push also fails CI it stops and comments, and that survives a restart. A later failure
// from a new human commit heals again: the guard keys on the failing commit's author, not a counter.
func TestAutoHealOneAttemptGuard(t *testing.T) {
	posted := make(chan string, 4)
	// commitAuthorEmail "agent@quack.local" - the failing commit IS quack's own.
	srv := stubFixGitHubFull(t, posted, []string{"quack:fix"}, true, gitCommitAuthorEmail, "someone-else")
	defer srv.Close()

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	chatID := globalChatID("github-acme-widgets-7")
	// Seed a prior cycle: sha1 (human) failed and quack dispatched a fix; sha2 below is that fix failing.
	if err := st.SetFixState(context.Background(), FixState{ChatID: chatID, LastSHA: "sha1"}); err != nil {
		t.Fatalf("seed fix state: %v", err)
	}
	ext, fh := newTestExtensionWithStore(t, srv.URL, []string{"ci_fix"}, st)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha2", 7)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	select {
	case c := <-posted:
		for _, want := range []string{"Auto-heal stopped", "no second attempt"} {
			if !strings.Contains(c, want) {
				t.Errorf("stop comment missing %q: %q", want, c)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no stop comment posted")
	}
	// The stop comment's branch returns right after posting it - beginFix is unreachable from there.
	if calls := fh.calls(); len(calls) != 0 {
		t.Error("must not dispatch a second fix for its own failing commit")
	}

	fs, err := ext.store.GetFixState(context.Background(), chatID)
	if err != nil || fs == nil || !fs.Stopped || fs.LastSHA != "sha2" {
		t.Fatalf("fix state = %+v, %v; want Stopped=true LastSHA=sha2", fs, err)
	}

	// "Restart": a fresh Extension over the same store. A sibling workflow
	// failing on the SAME commit stays silent (dedup, not a second stop comment).
	ext2, fh2 := newTestExtensionWithStore(t, srv.URL, []string{"ci_fix"}, st)
	rec2 := httptest.NewRecorder()
	ext2.handleWebhook(rec2, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha2", 7)))
	select {
	case <-fh2.notify:
		t.Error("stopped state must survive a restart; no run may dispatch")
	case c := <-posted:
		t.Errorf("stopped auto-heal posted a second comment for the same commit: %q", c)
	case <-time.After(150 * time.Millisecond):
		// silent dedup, as expected
	}

	// A NEW commit (a human's, not quack's) fails - auto-heal resumes with no
	// relabeling and no counter to reset.
	srv3 := stubFixGitHubFull(t, posted, []string{"quack:fix"}, true, "", "someone-else")
	defer srv3.Close()
	ext3, fh3 := newTestExtensionWithStore(t, srv3.URL, []string{"ci_fix"}, st)
	rec3 := httptest.NewRecorder()
	ext3.handleWebhook(rec3, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha3", 7)))
	fh3.waitForDispatch(t, 2*time.Second)
}

// On a quack-authored PR every commit is quack's, so the first-ever CI failure must still get a fix attempt,
// not read as "my own fix already failed" (autoHeal's st != nil gate).
func TestAutoHealAuthoredPRFirstFailureGetsAFix(t *testing.T) {
	posted := make(chan string, 4)
	srv := stubFixGitHubFull(t, posted, nil, true, gitCommitAuthorEmail, "quack[bot]")
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha1", 7)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	fh.waitForDispatch(t, 2*time.Second)
}

// stubFixGitHubDelayed delays the check-runs fetch to deterministically widen the window between
// autoHeal's GetFixState read and its SetFixState claim.
func stubFixGitHubDelayed(t *testing.T, posted chan<- string, prLabels []string, delay time.Duration) *httptest.Server {
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
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			time.Sleep(delay)
			fmt.Fprint(w, `{"check_runs":[{"id":42,"name":"go-test","conclusion":"failure","html_url":"https://ci/42","output":{"title":"tests failed","summary":"1 test failed"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/annotations"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[]`)
		case strings.Contains(r.URL.Path, "/commits/"):
			fmt.Fprint(w, `{"commit":{"author":{"email":""}}}`)
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
			fmt.Fprint(w, `{"title":"Test PR","body":"A test PR.","state":"open","head":{"ref":"feature-branch","sha":"headsha1"},"base":{"ref":"main"},"user":{"login":"someone-else"}}`)
		case isIssueMetaPath(r.URL.Path):
			fmt.Fprintf(w, `{"title":"Test PR","body":"A test PR.","state":"open","labels":[%s]}`, strings.Join(labelsJSON, ","))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

// Two workflow_run deliveries for the same head SHA must dispatch one fix, not race past GetFixState together.
// The success path posts no comment, so Host.Dispatch calls are the observable.
func TestAutoHealConcurrentSameHeadPostsOnce(t *testing.T) {
	posted := make(chan string, 8)
	srv := stubFixGitHubDelayed(t, posted, []string{"quack:fix"}, 50*time.Millisecond)
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

	body := workflowRunBody("completed", "failure", "sha1", 7)
	var p workflowRunPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var start, wg sync.WaitGroup
	start.Add(1)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			ext.autoHeal(p, 7, body)
		}()
	}
	start.Done()
	wg.Wait()

	if calls := fh.calls(); len(calls) != 1 {
		t.Fatalf("got %d Host.Dispatch calls for one head commit; want 1: %v", len(calls), calls)
	}
}

// Re-applying quack:fix re-arms auto-heal (clears a prior stop) and, with CI still failing, fixes immediately.
func TestFixLabelReapplyRearms(t *testing.T) {
	srv := stubFixGitHub(t, make(chan string, 4), []string{"quack:fix"}, true)
	defer srv.Close()

	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	chatID := globalChatID("github-acme-widgets-7")
	if err := st.SetFixState(context.Background(), FixState{ChatID: chatID, LastSHA: "headsha1", Stopped: true}); err != nil {
		t.Fatalf("seed fix state: %v", err)
	}

	ext, fh := newTestExtensionWithStore(t, srv.URL, []string{"ci_fix"}, st)

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("pull_request", pullRequestBody("labeled", "quack:fix")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)
	if !strings.Contains(req.Ask.Message, "commits on this PR's head branch that make the failing checks pass") {
		t.Errorf("re-armed run message = %q; want the fix deliverable", req.Ask.Message)
	}

	fs, err := ext.store.GetFixState(context.Background(), chatID)
	if err != nil {
		t.Fatalf("GetFixState: %v", err)
	}
	if fs == nil || fs.Stopped {
		t.Errorf("fix state = %+v; want the prior Stopped=true cleared", fs)
	}
}

// quack:fix on a PR with nothing failing arms silently (no review, no comment);
// a non-allowlisted sender is refused outright.
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
		srv := stubFixGitHub(t, make(chan string, 4), nil, true)
		defer srv.Close()
		ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("pull_request", fixLabelBody("alice")))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
		req := fh.waitForDispatch(t, 2*time.Second)
		for _, want := range []string{`<pull_request number="7">`, "commits on this PR's head branch that make the failing checks pass", "go-test"} {
			if !strings.Contains(req.Ask.Message, want) {
				t.Errorf("fix run message missing %q: %q", want, req.Ask.Message)
			}
		}
	})

	t.Run("nothing failing arms the flag silently, no phantom review", func(t *testing.T) {
		posted := make(chan string, 4)
		srv := stubFixGitHub(t, posted, nil, false)
		defer srv.Close()
		ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("pull_request", fixLabelBody("alice")))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
		select {
		case <-fh.notify:
			t.Error("no run should dispatch when nothing is failing (#655)")
		case c := <-posted:
			t.Errorf("no comment should be posted on a green PR: %q", c)
		case <-time.After(150 * time.Millisecond):
			// armed and silent, as expected
		}
	})

	t.Run("non-allowlisted sender is refused", func(t *testing.T) {
		srv := stubFixGitHub(t, make(chan string, 4), nil, true)
		defer srv.Close()
		ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("pull_request", fixLabelBody("mallory")))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200 no-op", rec.Code)
		}
		if calls := fh.calls(); len(calls) != 0 {
			t.Error("non-allowlisted sender must not dispatch")
		}
	})
}

// A CI-fix dispatch carries the ask-only worker background in Ask.NodeContext and only the
// failing check's annotations in Ask.ContextItems (go-test fails, lint is green: one NamedContext).
func TestWorkflowRunAttachesWorkerAskAndCIChecks(t *testing.T) {
	srv := stubFixGitHubFull(t, make(chan string, 4), []string{"quack:fix"}, true, "", "someone-else")
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha1", 7)))
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	req := fh.waitForDispatch(t, 2*time.Second)

	ask := req.Ask.NodeContext
	if strings.Contains(ask, "changed_files") {
		t.Errorf("worker ask must not carry the orchestrator's evidence:\n%s", ask)
	}
	if !strings.Contains(ask, `<pull_request number="7">`) {
		t.Errorf("worker ask missing the PR ask:\n%s", ask)
	}

	checks := req.Ask.ContextItems
	if len(checks) != 1 {
		t.Fatalf("ContextItems = %d entries, want exactly the one failing check (go-test; lint is green)", len(checks))
	}
	if checks[0].Name != "go-test" {
		t.Errorf("ContextItems[0].Name = %q, want go-test", checks[0].Name)
	}
	if !strings.Contains(checks[0].Detail, "TestFoo failed") {
		t.Errorf("ContextItems[0].Detail missing the check's own annotation:\n%s", checks[0].Detail)
	}
}

// CI builds the MERGE of head with base, so both Ask.Message and Ask.NodeContext must tell the worker
// to merge the PR's real base ("main") in and diagnose against that state.
func TestCIFixNamesTheMergeRef(t *testing.T) {
	assertMergeRef := func(t *testing.T, req sdk.DispatchRequest) {
		t.Helper()
		for _, field := range []struct {
			name string
			text string
		}{{"Ask.Message", req.Ask.Message}, {"Ask.NodeContext", req.Ask.NodeContext}} {
			for _, want := range []string{
				"GitHub Actions builds the MERGE", `base "main"`, `git merge "origin/main"`,
				"never report the checks as passing or deliver a no-op commit",
			} {
				if !strings.Contains(field.text, want) {
					t.Errorf("%s missing %q:\n%s", field.name, want, field.text)
				}
			}
		}
	}

	t.Run("workflow_run auto-heal", func(t *testing.T) {
		srv := stubFixGitHubFull(t, make(chan string, 4), []string{"quack:fix"}, true, "", "someone-else")
		defer srv.Close()
		ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("workflow_run", workflowRunBody("completed", "failure", "sha1", 7)))
		if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		assertMergeRef(t, fh.waitForDispatch(t, 2*time.Second))
	})

	t.Run("quack:fix label applied", func(t *testing.T) {
		fixLabelBody := []byte(`{
			"action":"labeled",
			"number":7,
			"pull_request":{"title":"Test PR","head":{"sha":"headsha1"}},
			"label":{"name":"quack:fix"},
			"repository":{"name":"widgets","owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},
			"installation":{"id":5},
			"sender":{"login":"alice"}
		}`)
		srv := stubFixGitHub(t, make(chan string, 4), nil, true)
		defer srv.Close()
		ext, fh := newTestExtension(t, srv.URL, []string{"ci_fix"})

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("pull_request", fixLabelBody))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
		assertMergeRef(t, fh.waitForDispatch(t, 2*time.Second))
	})

	t.Run("non-ci_fix dispatch carries no merge-ref instruction", func(t *testing.T) {
		srv := stubGitHub(t, make(chan string, 4))
		defer srv.Close()
		ext, fh := newTestExtension(t, srv.URL, []string{"mention"})

		rec := httptest.NewRecorder()
		ext.handleWebhook(rec, signedRequest("issue_comment", issueCommentBody("@quack add a feature")))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; want 202", rec.Code)
		}
		req := fh.waitForDispatch(t, 2*time.Second)
		if strings.Contains(req.Ask.Message, "<ci_ref>") {
			t.Errorf("a run with no checkSHA must not carry the CI merge-ref instruction:\n%s", req.Ask.Message)
		}
	})
}

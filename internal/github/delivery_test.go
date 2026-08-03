package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/vetting"
)

// newDeliveryApp returns an App wired to an httptest server for App.Deliver
// tests, its install/token caches pre-seeded so only the stubbed endpoints
// are hit (mirrors newReviewApp/seededApp in tools_test.go).
func newDeliveryApp(t *testing.T, handler http.HandlerFunc) *App {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL
	app.installs["acme/widgets"] = 1
	app.tokens[1] = cachedToken{token: "ghs_x", expires: time.Now().Add(time.Hour)}
	return app
}

// TestDeliverPullRequestUpdatesExistingInsteadOfDuplicate pins a staged
// pull_request delivered against a branch that already has an OPEN PR must
// UPDATE that PR, never open a second one.
func TestDeliverPullRequestUpdatesExistingInsteadOfDuplicate(t *testing.T) {
	var created bool
	var patchedBody map[string]string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			if !strings.Contains(r.URL.RawQuery, "head=acme:feature") || !strings.Contains(r.URL.RawQuery, "state=open") {
				t.Errorf("findOpenPR query = %q; want head=acme:feature&state=open", r.URL.RawQuery)
			}
			io.WriteString(w, `[{"number":42,"html_url":"https://github.com/acme/widgets/pull/42"}]`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &patchedBody)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/42"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			created = true
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/99","number":99}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := vetting.DeliveryContext{
		GatePassed: true, // these test delivery mechanics, not the gate caveat
		ChatID:     "chat-1",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature", // CloneDir empty ⇒ no real push attempted
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Add widget", Body: "does the thing"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if created {
		t.Error("a NEW pull request was opened despite an existing open PR for this branch")
	}
	if patchedBody["title"] != "Add widget" || patchedBody["body"] != "does the thing" {
		t.Errorf("existing PR was not updated with the staged title/body; patch body = %+v", patchedBody)
	}
	d, ok := takeDeliveryDetail("chat-1")
	if !ok || d.err != nil || d.prNumber != 42 {
		t.Errorf("recorded outcome = %+v, ok=%v; want the VERIFIED existing pr_number 42, no error", d, ok)
	}
}

// requireGitBinary skips a test when git isn't on PATH.
func requireGitBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH")
	}
}

// runGitTest runs one git command in dir, failing the test on error.
func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDeliverVerifiesPushAgainstGitHub pins a `git push` that exits 0
// is not proof the branch landed - Deliver must confirm the branch's head
// against GitHub's OWN state before opening/updating anything, and fail
// closed (no PR, no summary claiming success) when it doesn't match.
func TestDeliverVerifiesPushAgainstGitHub(t *testing.T) {
	requireGitBinary(t)

	bare := t.TempDir()
	runGitTest(t, bare, "init", "--bare", "--initial-branch=main")
	clone := t.TempDir()
	runGitTest(t, filepath.Dir(clone), "clone", "--quiet", bare, clone)
	runGitTest(t, clone, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(clone, "widget.go"), []byte("package widget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, clone, "add", "-A")
	runGitTest(t, clone, "-c", "user.name=test", "-c", "user.email=test@x.local", "commit", "--quiet", "-m", "add widget")
	fullSHA := runGitTest(t, clone, "rev-parse", "feature")

	dc := vetting.DeliveryContext{
		GatePassed: true, // these test delivery mechanics, not the gate caveat
		CloneURL:   "https://github.com/acme/widgets.git",
		CloneDir:   clone,
		Branch:     "feature",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Add widget", Body: "adds a widget"}},
	}

	t.Run("mismatched remote head fails closed", func(t *testing.T) {
		dc := dc
		dc.ChatID = "chat-push-fail"
		var prOpened bool
		app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/git/ref/heads/feature"):
				io.WriteString(w, `{"object":{"sha":"0000000000000000000000000000000000000000"}}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
				io.WriteString(w, `[]`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
				prOpened = true
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"html_url":"x","number":1}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})
		_, err := app.Deliver(context.Background(), t.TempDir(), dc)
		if err == nil {
			t.Fatal("expected an error when the pushed branch isn't reflected on GitHub")
		}
		if !strings.Contains(err.Error(), "not reflected") {
			t.Errorf("error = %v; want it to explain the head mismatch", err)
		}
		if prOpened {
			t.Error("a PR was opened despite failed push verification - must fail closed, never claim delivery")
		}
		d, ok := takeDeliveryDetail("chat-push-fail")
		if !ok || d.err == nil {
			t.Errorf("recorded outcome = %+v, ok=%v; want a recorded failure", d, ok)
		}
	})

	t.Run("verified remote head delivers", func(t *testing.T) {
		dc := dc
		dc.ChatID = "chat-push-ok"
		var prOpened bool
		app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/git/ref/heads/feature"):
				fmt.Fprintf(w, `{"object":{"sha":%q}}`, fullSHA)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
				io.WriteString(w, `[]`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
				prOpened = true
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/9","number":9}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})
		if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !prOpened {
			t.Error("expected a PR to be opened once the push was verified against GitHub")
		}
		d, ok := takeDeliveryDetail("chat-push-ok")
		if !ok || d.err != nil || d.pushedSHA != fullSHA || d.prNumber != 9 {
			t.Errorf("recorded outcome = %+v, ok=%v; want the verified pushed SHA + pr_number", d, ok)
		}
	})

	// #570: GitHub's git-refs API isn't read-your-writes consistent - a ref
	// lookup right after an accepted push can 404 before it settles. Delivery
	// must retry that instead of declaring the push a phantom failure.
	t.Run("transient 404 on verification recovers", func(t *testing.T) {
		dc := dc
		dc.ChatID = "chat-push-race"
		var refHits, prOpened int32
		app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/git/ref/heads/feature"):
				if atomic.AddInt32(&refHits, 1) <= 2 {
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
				fmt.Fprintf(w, `{"object":{"sha":%q}}`, fullSHA)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
				io.WriteString(w, `[]`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
				atomic.AddInt32(&prOpened, 1)
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/10","number":10}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})
		if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if atomic.LoadInt32(&prOpened) != 1 {
			t.Error("expected the PR to be opened once the racy verification finally landed")
		}
		d, ok := takeDeliveryDetail("chat-push-race")
		if !ok || d.err != nil || d.prNumber != 10 {
			t.Errorf("recorded outcome = %+v, ok=%v; want a successful delivery despite the transient 404s", d, ok)
		}
	})
}

// TestDeliverCommentIdempotentEdit pins re-delivering a staged comment
// for the SAME slot must EDIT the prior quack-authored comment carrying its
// marker, not pile up a duplicate.
func TestDeliverCommentIdempotentEdit(t *testing.T) {
	var posted, patched bool
	var patchedBody string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			io.WriteString(w, `[{"id":555,"node_id":"NODE555","body":"progress: 40%\n\n<!-- quack:delivery:comment:status -->","user":{"login":"quack[bot]"}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			posted = true
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/comments/555"):
			patched = true
			data, _ := io.ReadAll(r.Body)
			var b map[string]string
			_ = json.Unmarshal(data, &b)
			patchedBody = b["body"]
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := vetting.DeliveryContext{
		GatePassed:  true, // these test delivery mechanics, not the gate caveat
		ChatID:      "chat-comment",
		CloneURL:    "https://github.com/acme/widgets.git",
		IssueNumber: 7,
		Items:       []vetting.StagedDelivery{{Kind: "comment", Slot: "status", Body: "progress: 80%"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if posted {
		t.Error("a duplicate comment was POSTed despite a prior quack comment carrying the same slot marker")
	}
	if !patched {
		t.Fatal("the prior comment was not edited in place")
	}
	if !strings.Contains(patchedBody, "progress: 80%") || !strings.Contains(patchedBody, "quack:delivery:comment:status") {
		t.Errorf("patched body = %q; want the new content plus its marker", patchedBody)
	}
}

// TestDeliverCollapsesPriorReview pins review half: before submitting a
// new review, Deliver minimizes (GraphQL minimizeComment) any prior
// quack-authored review carrying the review marker.
func TestDeliverCollapsesPriorReview(t *testing.T) {
	var minimizedID string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[]`) // no inline comments drafted
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[{"node_id":"REVIEW1","body":"old findings\n\n<!-- quack:delivery:review -->","state":"COMMENTED","user":{"login":"quack[bot]"}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `{"id":2,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-2"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			data, _ := io.ReadAll(r.Body)
			var body struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(data, &body)
			minimizedID = body.Variables.ID
			io.WriteString(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := vetting.DeliveryContext{
		GatePassed:  true, // these test delivery mechanics, not the gate caveat
		ChatID:      "chat-review",
		CloneURL:    "https://github.com/acme/widgets.git",
		IssueNumber: 7,
		Items:       []vetting.StagedDelivery{{Kind: "review", Event: "comment", Body: "new findings"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if minimizedID != "REVIEW1" {
		t.Errorf("minimizeComment subjectId = %q; want the prior review's node_id REVIEW1", minimizedID)
	}
}

// A review on a PR quack authored can't carry an approve/request_changes
// verdict (GitHub 422s an author approving their own PR) - but a COMMENT-event
// review IS allowed, and #513 pins that it must still carry the findings as
// real inline comments[], not flattened text.
func TestDeliverReviewOnOwnPRIsCommentNoVerdict(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"quack[bot]"}}`) // quack authored this PR
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[{"filename":"main.go","patch":"@@ -42,1 +42,1 @@\n-old\n+new"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		case strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			t.Error("must deliver the own-PR review as a review, not a flattened issue comment")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := vetting.DeliveryContext{
		GatePassed: true, ChatID: "chat-ownpr", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []vetting.StagedDelivery{{Kind: "review", Event: "approve", Body: "clean change",
			Comments: []vetting.ReviewComment{{Path: "main.go", Line: 42, Body: "tiny nit"}}}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Event    string `json:"event"`
		Body     string `json:"body"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Event != "COMMENT" {
		t.Fatalf("review event = %q; want COMMENT (own PR can't carry approve/request_changes)", posted.Event)
	}
	if !strings.Contains(posted.Body, "clean change") {
		t.Fatalf("self-review body missing the summary:\n%s", posted.Body)
	}
	if !strings.Contains(posted.Body, "<!-- quack:delivery:review:approve -->") {
		t.Fatalf("self-review body missing its verdict marker (needed by the quack:merge gate, #482):\n%s", posted.Body)
	}
	if len(posted.Comments) != 1 || posted.Comments[0].Path != "main.go" || posted.Comments[0].Line != 42 {
		t.Fatalf("finding did not land as an inline review comment (#513): %s", reviewBody)
	}
}

// TestDeliverReviewOnOwnPRStripsVerdictTail pins #482: the raw ACP reviewer
// answer carries a machine-parseable VERDICT/FINDINGS tail (for
// augmentFromAnswer) and sometimes a fallback-format preamble - neither
// belongs in the human-facing own-PR review body.
func TestDeliverReviewOnOwnPRStripsVerdictTail(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"quack[bot]"}}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[{"filename":"main.go","patch":"@@ -42,1 +42,1 @@\n-old\n+new"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		case strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			t.Error("must deliver the own-PR review as a review, not a flattened issue comment")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	rawAnswer := "Since staging tools aren't available in this environment, here is the full structured review as the fallback output format:\n\n" +
		"This change looks solid overall.\n\n" +
		"VERDICT: approve\n" +
		"FINDINGS:\n" +
		"- main.go:42: tiny nit\n"
	dc := vetting.DeliveryContext{
		GatePassed: true, ChatID: "chat-ownpr-tail", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []vetting.StagedDelivery{{Kind: "review", Event: "approve", Body: rawAnswer,
			Comments: []vetting.ReviewComment{{Path: "main.go", Line: 42, Body: "tiny nit"}}}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body     string `json:"body"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(posted.Body, "This change looks solid overall.") {
		t.Fatalf("self-review body dropped the human-facing summary:\n%s", posted.Body)
	}
	if strings.Contains(posted.Body, "VERDICT:") || strings.Contains(posted.Body, "FINDINGS:") {
		t.Fatalf("self-review body leaked the machine-parseable tail:\n%s", posted.Body)
	}
	if strings.Contains(posted.Body, "fallback output format") {
		t.Fatalf("self-review body leaked the fallback-format preamble:\n%s", posted.Body)
	}
	if !strings.Contains(posted.Body, "<!-- quack:delivery:review:approve -->") {
		t.Fatalf("self-review body missing its verdict marker:\n%s", posted.Body)
	}
	if len(posted.Comments) != 1 || posted.Comments[0].Path != "main.go" || posted.Comments[0].Line != 42 {
		t.Fatalf("finding did not land as an inline review comment (#513): %s", reviewBody)
	}
}

// An external (ACP) reviewer's staged review carries gate-parsed inline
// comments and no ledger PR number - delivery posts the comments and recovers
// the PR from the GitHub-dispatched chat id.
func TestDeliverReviewInlineCommentsAndChatIDPR(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			// main.go line 42 commentable on the RIGHT side.
			io.WriteString(w, `[{"filename":"main.go","patch":"@@ -42,1 +42,1 @@\n-old\n+new"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-7", // the webhook dispatch session id - the PR number source
		CloneURL:   "https://github.com/acme/widgets.git",
		Items: []vetting.StagedDelivery{{
			Kind: "review", Event: "request_changes", Body: "two blockers",
			Comments: []vetting.ReviewComment{{Path: "main.go", Line: 42, Body: "route shadowed"}},
		}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if len(posted.Comments) != 1 || posted.Comments[0].Path != "main.go" || posted.Comments[0].Line != 42 {
		t.Fatalf("inline comments not posted: %s", reviewBody)
	}
}

// TestDeliverReviewNeverPushesBranch pins #452: a review-only delivery whose
// context carries a Branch + CloneDir (a setup-provisioned reviewer node always
// does) must NOT push - a review lands on the existing PR via the API. Before
// the stagesPush guard, Deliver force-pushed the reviewer's (base-HEAD) branch,
// resetting the reviewed PR and wiping its commits. CloneDir here is a non-git
// dir: the OLD code would try to push it and error; the fix skips push entirely.
func TestDeliverReviewNeverPushesBranch(t *testing.T) {
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		case strings.Contains(r.URL.Path, "/git/ref/"):
			// The push-verify endpoint - reaching it means a push was attempted.
			t.Errorf("review delivery must never push/verify a branch, got %s %s", r.Method, r.URL.Path)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-7",
		CloneURL:   "https://github.com/acme/widgets.git",
		CloneDir:   t.TempDir(), // set, as a real reviewer node's is - must still not push
		Branch:     "some-pr-branch",
		Items:      []vetting.StagedDelivery{{Kind: "review", Event: "approve", Body: "looks good"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("review-only Deliver should succeed without any push: %v", err)
	}
}

// A gate FAIL still delivers the PR (a human decides) but opens it as a DRAFT
// so it cannot be merged accidentally.
func TestDeliverFailedGateOpensDraftPR(t *testing.T) {
	var prBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`) // no existing open PR
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/3"):
			io.WriteString(w, `{"title":"t","body":"b","state":"open","labels":[]}`) // withClosesTrailer's partial-fix check
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			prBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/8","number":8}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := vetting.DeliveryContext{
		GatePassed:   false,
		GateFeedback: "tests fail",
		ChatID:       "github-acme-widgets-3",
		CloneURL:     "https://github.com/acme/widgets.git",
		Branch:       "quack/fix",
		Items:        []vetting.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "the fix"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Draft bool   `json:"draft"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(prBody, &posted); err != nil {
		t.Fatal(err)
	}
	if !posted.Draft {
		t.Fatalf("gate-failed PR must open as a draft: %s", prBody)
	}
	if !strings.Contains(posted.Body, "did NOT pass") {
		t.Fatalf("caveat banner missing from body: %s", posted.Body)
	}
	// #575: a fresh PR opened for a chat tied to issue #3 closes it deterministically.
	if !strings.Contains(posted.Body, "Closes #3") {
		t.Fatalf("delivered PR body missing deterministic Closes trailer: %s", posted.Body)
	}
}

// TestDeliverSuppressesClosesTrailerWhenPartialFix pins #575's "Done when":
// the quack:partial-fix label on the originating issue must suppress the
// deterministic trailer - a maintainer's explicit "this PR does not close it"
// signal, never overridden.
func TestDeliverSuppressesClosesTrailerWhenPartialFix(t *testing.T) {
	var prBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`) // no existing open PR
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9"):
			io.WriteString(w, `{"title":"t","body":"b","state":"open","labels":[{"name":"quack:partial-fix"}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			prBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/10","number":10}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-9",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/partial",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Partial fix", Body: "does part of it"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(prBody, &posted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(posted.Body, "Closes #9") {
		t.Fatalf("partial-fix issue must not get an unconditional Closes trailer: %s", posted.Body)
	}
}

// TestDeliverSkipsClosesTrailerWhenChatIDResolvesToAPR covers the edge case
// findOpenPR alone can't catch: a PR-scoped chat id (github-owner-repo-<PR
// number>) whose branch's ORIGINAL PR was since closed/merged also takes the
// fresh-open path (no OPEN PR on that branch), and the chat id's number is
// still a pull request, not an issue - GitHub's issues endpoint returns PRs
// too, so a body-less partial-fix check alone would wrongly close #92 by
// naming another pull request.
func TestDeliverSkipsClosesTrailerWhenChatIDResolvesToAPR(t *testing.T) {
	var prBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`) // no OPEN PR on this branch - the original PR #92 was closed/merged
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/92"):
			// GitHub marks an issues-endpoint response as actually being a PR via
			// the pull_request field.
			io.WriteString(w, `{"title":"t","body":"b","state":"closed","pull_request":{},"labels":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			prBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/93","number":93}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-92",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/fix-92",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Redo the fix", Body: "the fix"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(prBody, &posted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(posted.Body, "Closes") {
		t.Fatalf("chat id resolving to a PULL REQUEST must never get a Closes trailer: %s", posted.Body)
	}
}

// TestDeliverDoesNotAppendClosesOnPRUpdate pins the other half of #575: a PR
// update (an already-open PR on this branch - a fix/continuation run, not a
// fresh issue-implement run) must never get a Closes trailer, even when the
// chat id encodes a number - that number is the PR's own, not a distinct
// issue to close.
func TestDeliverDoesNotAppendClosesOnPRUpdate(t *testing.T) {
	var patchedBody map[string]string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[{"number":11,"html_url":"https://github.com/acme/widgets/pull/11"}]`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/pulls/11"):
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &patchedBody)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/11"}`)
		case strings.HasSuffix(r.URL.Path, "/issues/11"):
			t.Error("an update to an existing PR must never look up the partial-fix label")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-11", // same number as the PR itself
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "fix/11-something",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "the fix"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if strings.Contains(patchedBody["body"], "Closes") {
		t.Fatalf("PR update must never gain a Closes trailer: %+v", patchedBody)
	}
}

// TestDeliverClosesTrailerNotDuplicated pins the other "Done when": a body
// that already references the issue with a closing keyword is left alone -
// no second trailer, and no partial-fix lookup needed to decide that.
func TestDeliverClosesTrailerNotDuplicated(t *testing.T) {
	var prBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/issues/5"):
			t.Error("a body that already closes the issue must not trigger a partial-fix lookup")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			prBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/6","number":6}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-5",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/fix5",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "does the work.\n\nCloses #5\n"}},
	}
	if _, err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(prBody, &posted); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(posted.Body, "Closes #5"); n != 1 {
		t.Fatalf("Closes #5 appears %d times, want exactly 1: %s", n, posted.Body)
	}
}

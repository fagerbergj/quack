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
// is not proof the branch landed — Deliver must confirm the branch's head
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
			t.Error("a PR was opened despite failed push verification — must fail closed, never claim delivery")
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

// A review on a PR quack authored can't carry a verdict (GitHub 422s an author
// approving their own PR), so it delivers as a plain comment — never a
// submit_review — still carrying the findings.
func TestDeliverReviewOnOwnPRIsCommentNoVerdict(t *testing.T) {
	var commentBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"quack[bot]"}}`) // quack authored this PR
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			commentBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":1,"html_url":"https://github.com/acme/widgets/pull/7#issuecomment-1"}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			t.Error("must not submit a formal review verdict on a PR quack authored")
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
	body := decodedCommentBody(t, commentBody)
	if !strings.Contains(body, "clean change") || !strings.Contains(body, "main.go:42") {
		t.Fatalf("self-review comment missing the review body/findings:\n%s", body)
	}
	if !strings.Contains(body, "<!-- quack:delivery:review:approve -->") {
		t.Fatalf("self-review comment missing its verdict marker (needed by the quack:merge gate, #482):\n%s", body)
	}
}

// decodedCommentBody extracts the human-readable "body" field from a
// github_issue_comment POST payload — json.Marshal HTML-escapes the delivery
// marker's `<`/`>`, so tests must decode rather than substring-match the raw
// wire bytes.
func decodedCommentBody(t *testing.T, raw []byte) string {
	t.Helper()
	var v struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode posted comment: %v\nraw: %s", err, raw)
	}
	return v.Body
}

// TestDeliverReviewOnOwnPRStripsVerdictTail pins #482: the raw ACP reviewer
// answer carries a machine-parseable VERDICT/FINDINGS tail (for
// augmentFromAnswer) and sometimes a fallback-format preamble — neither
// belongs in the human-facing own-PR comment.
func TestDeliverReviewOnOwnPRStripsVerdictTail(t *testing.T) {
	var commentBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"quack[bot]"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			commentBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":1,"html_url":"https://github.com/acme/widgets/pull/7#issuecomment-1"}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			t.Error("must not submit a formal review verdict on a PR quack authored")
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
	body := decodedCommentBody(t, commentBody)
	if !strings.Contains(body, "This change looks solid overall.") {
		t.Fatalf("self-review comment dropped the human-facing summary:\n%s", body)
	}
	if strings.Contains(body, "VERDICT:") || strings.Contains(body, "FINDINGS:") {
		t.Fatalf("self-review comment leaked the machine-parseable tail:\n%s", body)
	}
	if strings.Contains(body, "fallback output format") {
		t.Fatalf("self-review comment leaked the fallback-format preamble:\n%s", body)
	}
	if !strings.Contains(body, "main.go:42") {
		t.Fatalf("self-review comment dropped the rendered inline finding:\n%s", body)
	}
	if !strings.Contains(body, "<!-- quack:delivery:review:approve -->") {
		t.Fatalf("self-review comment missing its verdict marker:\n%s", body)
	}
}

// An external (ACP) reviewer's staged review carries gate-parsed inline
// comments and no ledger PR number — delivery posts the comments and recovers
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
		ChatID:     "github-acme-widgets-7", // the webhook dispatch session id — the PR number source
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
// does) must NOT push — a review lands on the existing PR via the API. Before
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
			// The push-verify endpoint — reaching it means a push was attempted.
			t.Errorf("review delivery must never push/verify a branch, got %s %s", r.Method, r.URL.Path)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-7",
		CloneURL:   "https://github.com/acme/widgets.git",
		CloneDir:   t.TempDir(), // set, as a real reviewer node's is — must still not push
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
}

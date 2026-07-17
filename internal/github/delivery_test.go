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
	if err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
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
		err := app.Deliver(context.Background(), t.TempDir(), dc)
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
		if err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
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
	if err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
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
	if err := app.Deliver(context.Background(), t.TempDir(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if minimizedID != "REVIEW1" {
		t.Errorf("minimizeComment subjectId = %q; want the prior review's node_id REVIEW1", minimizedID)
	}
}

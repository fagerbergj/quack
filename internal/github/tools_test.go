package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/vetting"
)

// samplePatch is a unified diff for auth.go: hunk starts at new-file line 40, so
// commentable RIGHT lines are 40 (context), 41 (added), 42 (added), 43 (context);
// old-file (LEFT) lines 40 and 41 are the two context lines.
const samplePatch = "@@ -40,2 +40,4 @@ func Check() {\n ctx := r.Context()\n+\tuser := lookup(ctx)\n+\tif user == nil { panic(user) }\n \treturn user"

// newReviewApp returns an App wired to an httptest server that stubs
// GET /pulls/7/files (one changed file, auth.go) and, if provided, the
// POST /pulls/7/reviews endpoint. Installation/token endpoints are bypassed by
// seeding the App caches, so the tools only hit the stubbed endpoints.
func newReviewApp(t *testing.T, reviews http.HandlerFunc) *App {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/7/files", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"filename": "auth.go", "patch": samplePatch}})
	})
	if reviews != nil {
		mux.HandleFunc("/repos/acme/widgets/pulls/7/reviews", reviews)
	}
	srv := httptest.NewServer(mux)
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

func TestSubmitReviewValidatesEvent(t *testing.T) {
	app := newReviewApp(t, func(http.ResponseWriter, *http.Request) {
		t.Error("no review POST should be made for an invalid event")
	})
	if _, err := app.submitReview(context.Background(), submitReviewArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Event: "LGTM"}); err == nil {
		t.Fatal("expected an error for a bad event")
	}
}

func TestParsePatch(t *testing.T) {
	pos := parsePatch(samplePatch)
	for _, ln := range []int{40, 41, 42, 43} {
		if !pos.right[ln] {
			t.Errorf("RIGHT line %d should be commentable", ln)
		}
	}
	if pos.right[44] {
		t.Error("line 44 is past the hunk; should not be commentable")
	}
	if !pos.left[40] || !pos.left[41] {
		t.Errorf("LEFT positions = %v; want 40 and 41 present", pos.left)
	}
}

// seededApp returns an App whose install/token caches are primed for acme/widgets
// and whose apiBase points at handler, so PR-discussion tools hit only handler.
func seededApp(t *testing.T, handler http.HandlerFunc) *App {
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

func TestListPRDiscussion(t *testing.T) {
	app := seededApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/7/comments"):
			_, _ = io.WriteString(w, `[{"id":11,"path":"auth.go","line":42,"body":"nit","user":{"login":"bob"},"in_reply_to_id":0,"created_at":"2026-01-01T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			_, _ = io.WriteString(w, `[{"id":22,"body":"thanks!","user":{"login":"alice"},"created_at":"2026-01-02T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			_, _ = io.WriteString(w, `[{"id":33,"body":"looks good","state":"APPROVED","user":{"login":"carol"},"submitted_at":"2026-01-03T00:00:00Z"}]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	d, err := app.listPRDiscussion(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("listPRDiscussion: %v", err)
	}
	if len(d.ReviewComments) != 1 || d.ReviewComments[0].User != "bob" || d.ReviewComments[0].Line != 42 {
		t.Errorf("review comments = %+v", d.ReviewComments)
	}
	if len(d.Comments) != 1 || d.Comments[0].User != "alice" || d.Comments[0].Body != "thanks!" {
		t.Errorf("comments = %+v", d.Comments)
	}
	if len(d.Reviews) != 1 || d.Reviews[0].State != "APPROVED" || d.Reviews[0].User != "carol" {
		t.Errorf("reviews = %+v", d.Reviews)
	}
}

func TestReplyToReviewComment(t *testing.T) {
	var gotPath, gotBody string
	app := seededApp(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") || strings.HasSuffix(r.URL.Path, "/installation") {
			_, _ = io.WriteString(w, `{"id":1,"token":"ghs_x","expires_at":"2999-01-01T00:00:00Z"}`)
			return
		}
		gotPath = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":88,"html_url":"https://github.com/acme/widgets/pull/7#discussion_r88"}`)
	})

	res, err := app.reply(context.Background(), replyArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, CommentID: 11, Body: "agreed, fixing"})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if gotPath != "/repos/acme/widgets/pulls/7/comments/11/replies" {
		t.Errorf("reply path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"body":"agreed, fixing"`) {
		t.Errorf("reply body = %q", gotBody)
	}
	if res.ID != 88 {
		t.Errorf("reply id = %d; want 88", res.ID)
	}
}

func TestReactToComment(t *testing.T) {
	var gotPath, gotBody string
	app := seededApp(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":7}`)
	})

	// review_comment → pulls/comments/{id}/reactions
	if _, err := app.react(context.Background(), reactArgs{Owner: "acme", Repo: "widgets", CommentID: 11, CommentType: "review_comment", Content: "rocket"}); err != nil {
		t.Fatalf("react: %v", err)
	}
	if gotPath != "/repos/acme/widgets/pulls/comments/11/reactions" || !strings.Contains(gotBody, `"content":"rocket"`) {
		t.Errorf("review-comment reaction: path=%q body=%q", gotPath, gotBody)
	}

	// issue_comment → issues/comments/{id}/reactions
	if _, err := app.react(context.Background(), reactArgs{Owner: "acme", Repo: "widgets", CommentID: 22, CommentType: "issue_comment", Content: "eyes"}); err != nil {
		t.Fatalf("react issue: %v", err)
	}
	if gotPath != "/repos/acme/widgets/issues/comments/22/reactions" {
		t.Errorf("issue-comment reaction path = %q", gotPath)
	}

	// Invalid content and type are rejected before any HTTP call.
	if _, err := app.react(context.Background(), reactArgs{Owner: "acme", Repo: "widgets", CommentID: 1, CommentType: "review_comment", Content: "thumbsup"}); err == nil {
		t.Error("expected error for a bad reaction content")
	}
	if _, err := app.react(context.Background(), reactArgs{Owner: "acme", Repo: "widgets", CommentID: 1, CommentType: "nope", Content: "eyes"}); err == nil {
		t.Error("expected error for a bad comment_type")
	}
}

// newFilesApp is newReviewApp with an arbitrary set of changed files (each
// carrying samplePatch), for the path-normalisation cases.
func newFilesApp(t *testing.T, filenames ...string) *App {
	t.Helper()
	files := make([]map[string]string, len(filenames))
	for i, f := range filenames {
		files[i] = map[string]string{"filename": f, "patch": samplePatch}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/7/files", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(files)
	})
	srv := httptest.NewServer(mux)
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

// TestReviewToolsRegistered guards that all four review tools plus the existing
// two are wired into Tools().
func TestReviewToolsRegistered(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	want := map[string]bool{
		"github_comment":                 false,
		"github_reply_to_review_comment": false,
		"github_react_to_comment":        false,
	}
	for _, tl := range app.Tools() {
		want[tl.Name()] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q not registered in Tools()", name)
		}
	}
}

// TestPullRequestAndSubmitReviewAreNotModelTools pins the staged-delivery
// spine's core safety property: opening a PR and submitting a review make work
// PUBLIC, so no agent tool call can do either anymore - only the harness's own
// delivery step (internal/github/webhook.go), via createPullRequest/
// createReview, does that, and only after a judge pass.
func TestPullRequestAndSubmitReviewAreNotModelTools(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	for _, tl := range app.Tools() {
		if tl.Name() == "github_pull_request" || tl.Name() == "github_submit_review" {
			t.Errorf("%q must not be a model-facing tool anymore", tl.Name())
		}
	}
}

// validComments is the delivery-time replacement for the draft tools' per-add
// validation: gate-parsed inline findings are anchored against the PR diff, a
// clone-relative path is normalised to its repo-relative form, and anything
// unanchorable is DROPPED (the summary body still carries the finding) instead
// of 422-ing the whole review.
func TestValidCommentsNormalisesAndDrops(t *testing.T) {
	app := newReviewApp(t, nil)
	in := []vetting.ReviewComment{
		{Path: "auth.go", Line: 42, Body: "exact path, commentable line"},
		{Path: "widgets/auth.go", Line: 42, Body: "clone-relative path"},
		{Path: "auth.go", Line: 999, Body: "uncommentable line"},
		{Path: "nope.go", Line: 42, Body: "not a changed file"},
	}
	got := app.validComments(context.Background(), "acme", "widgets", 7, in)
	if len(got) != 2 {
		t.Fatalf("validComments kept %d, want 2: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Path != "auth.go" || c.Line != 42 {
			t.Errorf("comment not normalised to auth.go:42: %+v", c)
		}
	}
}

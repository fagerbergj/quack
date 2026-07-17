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

func TestAddReviewCommentValidatesLocation(t *testing.T) {
	app := newReviewApp(t, nil)
	ctx := context.Background()

	// Valid: line 42 is an added line on the RIGHT side of auth.go.
	res, err := app.addReviewComment(ctx, addReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Path: "auth.go", Line: 42, Body: "nil deref risk"})
	if err != nil {
		t.Fatalf("valid add: %v", err)
	}
	if res.DraftCount != 1 || res.Index != 0 {
		t.Errorf("result = %+v; want index 0, draft_count 1", res)
	}
	if got := app.draftList("acme", "widgets", 7); len(got) != 1 || got[0].Line != 42 {
		t.Errorf("draft = %+v; want one comment on line 42", got)
	}

	// Invalid line: 999 is not in any hunk → rejected, naming the valid range.
	_, err = app.addReviewComment(ctx, addReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Path: "auth.go", Line: 999, Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "not commentable") || !strings.Contains(err.Error(), "40") {
		t.Errorf("expected off-diff line rejection naming valid lines; got %v", err)
	}

	// Invalid path: not a changed file → rejected.
	_, err = app.addReviewComment(ctx, addReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Path: "nope.go", Line: 1, Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "not a changed file") {
		t.Errorf("expected changed-file rejection; got %v", err)
	}

	// Rejected comments were NOT added — draft still holds only the valid one.
	if got := app.draftList("acme", "widgets", 7); len(got) != 1 {
		t.Errorf("draft size = %d; want 1 (rejects not drafted)", len(got))
	}
}

func TestListAndDeleteReviewComments(t *testing.T) {
	app := newReviewApp(t, nil)
	ctx := context.Background()
	for _, ln := range []int{41, 42} {
		if _, err := app.addReviewComment(ctx, addReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Path: "auth.go", Line: ln, Body: "note"}); err != nil {
			t.Fatalf("add line %d: %v", ln, err)
		}
	}

	list, err := app.listReviewComments(draftPRArgs{Owner: "acme", Repo: "widgets", PullNumber: 7})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Comments) != 2 || list.Comments[0].Index != 0 || list.Comments[1].Line != 42 {
		t.Errorf("list = %+v; want two drafted comments indexed 0,1", list.Comments)
	}

	del, err := app.deleteReviewComment(deleteReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Index: 0})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !del.Deleted || del.DraftCount != 1 {
		t.Errorf("delete result = %+v; want deleted, draft_count 1", del)
	}
	if got := app.draftList("acme", "widgets", 7); len(got) != 1 || got[0].Line != 42 {
		t.Errorf("after delete draft = %+v; want the line-42 comment", got)
	}

	// Deleting an out-of-range index errors.
	if _, err := app.deleteReviewComment(deleteReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Index: 9}); err == nil {
		t.Error("expected error deleting a missing index")
	}
}

func TestSubmitReviewPostsOneReviewAndClearsDraft(t *testing.T) {
	var gotBody map[string]any
	app := newReviewApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "token ghs_x" {
			t.Errorf("Authorization = %q; want token ghs_x", got)
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &gotBody)
		_, _ = io.WriteString(w, `{"id":555,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-555"}`)
	})
	ctx := context.Background()

	for _, ln := range []int{41, 42} {
		if _, err := app.addReviewComment(ctx, addReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Path: "auth.go", Line: ln, Body: "note"}); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	res, err := app.submitReview(ctx, submitReviewArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Event: "request_changes", Body: "Please fix."})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if gotBody["event"] != "REQUEST_CHANGES" {
		t.Errorf("event = %v; want REQUEST_CHANGES (normalised)", gotBody["event"])
	}
	// The delivery marker is appended's later collapse lookup — the
	// human-authored summary must still lead the body untouched.
	if body, _ := gotBody["body"].(string); !strings.HasPrefix(body, "Please fix.") || !strings.Contains(body, "<!-- quack:delivery:review -->") {
		t.Errorf("body = %v; want it to start with the summary and carry the delivery marker", gotBody["body"])
	}
	comments, ok := gotBody["comments"].([]any)
	if !ok || len(comments) != 2 {
		t.Fatalf("comments in review = %v; want 2 in one review", gotBody["comments"])
	}
	c0 := comments[0].(map[string]any)
	if c0["path"] != "auth.go" || c0["line"].(float64) != 41 || c0["body"] != "note" {
		t.Errorf("comment[0] = %v", c0)
	}
	if _, present := c0["side"]; present { // RIGHT default omitted
		t.Errorf("side should be omitted for default RIGHT; got %v", c0["side"])
	}
	if res.Comments != 2 || res.ReviewID != 555 {
		t.Errorf("result = %+v; want 2 comments, review 555", res)
	}

	// Draft cleared after a successful submit.
	if got := app.draftList("acme", "widgets", 7); len(got) != 0 {
		t.Errorf("draft after submit = %+v; want empty", got)
	}
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

// TestAddReviewCommentNormalisesWorkspacePath: the agent works inside a clone
// directory in its workspace, so it names files by their WORKSPACE path
// ("games/app/.../game.ts"); the PR diff addresses them repo-relative
// ("app/.../game.ts"). A unique suffix match must be accepted and normalised.
func TestAddReviewCommentNormalisesWorkspacePath(t *testing.T) {
	const repoPath = "app/games/flappy-bird/lib/game.ts"
	app := newFilesApp(t, repoPath)
	ctx := context.Background()

	for _, given := range []string{
		"games/" + repoPath, // clone-dir prefix
		"./" + repoPath,     // ./ prefix
		"lib/game.ts",       // a trailing fragment of the repo path
	} {
		app.draftTake("acme", "widgets", 7) // clear
		res, err := app.addReviewComment(ctx, addReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Path: given, Line: 42, Body: "bird never falls"})
		if err != nil {
			t.Fatalf("path %q: %v", given, err)
		}
		if res.DraftCount != 1 {
			t.Fatalf("path %q: draft_count = %d; want 1", given, res.DraftCount)
		}
		if got := app.draftList("acme", "widgets", 7); got[0].Path != repoPath {
			t.Errorf("path %q: drafted path = %q; want normalised %q", given, got[0].Path, repoPath)
		}
	}
}

// TestAddReviewCommentAmbiguousPath: two changed files match the suffix → don't
// guess; reject naming both candidates.
func TestAddReviewCommentAmbiguousPath(t *testing.T) {
	app := newFilesApp(t, "a/lib/game.ts", "b/lib/game.ts")
	_, err := app.addReviewComment(context.Background(), addReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Path: "lib/game.ts", Line: 42, Body: "x"})
	if err == nil {
		t.Fatal("expected an ambiguity rejection")
	}
	for _, want := range []string{"a/lib/game.ts", "b/lib/game.ts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not name candidate %q", err, want)
		}
	}
}

// TestAddReviewCommentUnknownPathListsChangedFiles: an unresolvable path must
// produce an error the model can ACT on — say the path is repo-relative and list
// the PR's changed files.
func TestAddReviewCommentUnknownPathListsChangedFiles(t *testing.T) {
	app := newFilesApp(t, "app/one.ts", "app/two.ts")
	_, err := app.addReviewComment(context.Background(), addReviewCommentArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, Path: "src/other.ts", Line: 42, Body: "x"})
	if err == nil {
		t.Fatal("expected a rejection for an unknown path")
	}
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "repo-relative") {
		t.Errorf("error must tell the model paths are repo-relative; got %v", err)
	}
	for _, want := range []string{"app/one.ts", "app/two.ts"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %v does not list changed file %q", err, want)
		}
	}
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
		"github_comment":            false,
		"github_add_review_comment": false, "github_list_review_comments": false,
		"github_delete_review_comment": false,
		"github_list_pr_comments":      false, "github_reply_to_review_comment": false,
		"github_react_to_comment": false,
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
// PUBLIC, so no agent tool call can do either anymore — only the harness's own
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

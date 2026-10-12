package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// samplePatch: hunk starts at new line 40, so commentable RIGHT lines are 40-43 (41, 42 added);
// LEFT lines 40 and 41 are the two context lines.
const samplePatch = "@@ -40,2 +40,4 @@ func Check() {\n ctx := r.Context()\n+\tuser := lookup(ctx)\n+\tif user == nil { panic(user) }\n \treturn user"

// newReviewApp stubs GET /pulls/7/files (auth.go) and optionally POST /pulls/7/reviews,
// with seeded caches so only those endpoints are hit.
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

// TestReviewToolsRegistered guards that the model-facing tools are wired into Tools().
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

// Opening a PR or submitting a review makes work public, so neither is a model tool:
// only the harness's Deliver does that, after a judge pass.
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

// validComments anchors findings against the diff, normalises clone-relative paths, and re-anchors an
// uncommentable line to the nearest commentable one; only a path outside the diff is dropped.
func TestValidCommentsNormalisesAndReanchors(t *testing.T) {
	app := newReviewApp(t, nil)
	in := []sdk.ReviewComment{
		{Path: "auth.go", Line: 42, Body: "exact path, commentable line"},
		{Path: "widgets/auth.go", Line: 42, Body: "clone-relative path"},
		{Path: "auth.go", Line: 999, Body: "uncommentable line"},
		{Path: "nope.go", Line: 42, Body: "not a changed file"},
	}
	inline, unanchored := app.validComments(context.Background(), "acme", "widgets", 7, in)
	if len(inline) != 3 {
		t.Fatalf("validComments kept %d inline, want 3 (nope.go dropped, the other three survive): %+v", len(inline), inline)
	}
	if len(unanchored) != 0 {
		t.Fatalf("validComments left %d unanchored, want 0 (auth.go has commentable lines to re-anchor onto): %+v", len(unanchored), unanchored)
	}
	var reanchored *reviewComment
	for i := range inline {
		if inline[i].Path != "auth.go" {
			t.Errorf("comment not normalised to auth.go: %+v", inline[i])
		}
		if inline[i].Body == "uncommentable line" || strings.Contains(inline[i].Body, "line 999") {
			reanchored = &inline[i]
		}
	}
	if reanchored == nil {
		t.Fatalf("the uncommentable-line finding should have survived, re-anchored: %+v", inline)
	}
	if reanchored.Line != 43 {
		t.Errorf("re-anchored line = %d, want 43 (nearest commentable line to 999 in auth.go)", reanchored.Line)
	}
	if !strings.Contains(reanchored.Body, "line 999") {
		t.Errorf("re-anchored body doesn't state the true location: %q", reanchored.Body)
	}
}

// A finding on a pure-deletion hunk can't be re-anchored, so it returns as unanchored for the caller
// to fold into the review body instead of vanishing.
func TestValidCommentsUnanchoredWhenFileHasNoCommentableLine(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls/7/files", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"filename": "deleted.go", "patch": "@@ -10,3 +10,0 @@\n-a\n-b\n-c"},
		})
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

	in := []sdk.ReviewComment{{Path: "deleted.go", Line: 50, Body: "double instantiation causes deletions not to reflect in the list"}}
	inline, unanchored := app.validComments(context.Background(), "acme", "widgets", 7, in)
	if len(inline) != 0 {
		t.Fatalf("validComments kept %d inline, want 0 (deleted.go has no commentable RIGHT line): %+v", len(inline), inline)
	}
	if len(unanchored) != 1 {
		t.Fatalf("validComments left %d unanchored, want 1: %+v", len(unanchored), unanchored)
	}
	if unanchored[0].Path != "deleted.go" || unanchored[0].Line != 50 || unanchored[0].Body != "double instantiation causes deletions not to reflect in the list" {
		t.Errorf("unanchored finding lost its identity: %+v", unanchored[0])
	}

	rendered := renderUnanchoredFindings(unanchored)
	if !strings.Contains(rendered, "deleted.go") || !strings.Contains(rendered, "line 50") {
		t.Errorf("rendered block doesn't identify the finding's true location: %q", rendered)
	}
	if !strings.Contains(rendered, "double instantiation") {
		t.Errorf("rendered block dropped the finding text: %q", rendered)
	}
}

// A byte-identical finding staged twice posts once; two different findings on one line both survive.
func TestValidCommentsDropsExactDuplicates(t *testing.T) {
	app := newReviewApp(t, nil)
	in := []sdk.ReviewComment{
		{Path: "auth.go", Line: 42, Body: "blocking: nil deref"},
		{Path: "auth.go", Line: 42, Body: "blocking: nil deref"}, // exact repeat
		{Path: "auth.go", Line: 42, Body: "suggestion: extract helper"},
	}
	got, unanchored := app.validComments(context.Background(), "acme", "widgets", 7, in)
	if len(got) != 2 {
		t.Fatalf("validComments kept %d, want 2 (one exact dup dropped): %+v", len(got), got)
	}
	if len(unanchored) != 0 {
		t.Fatalf("validComments left %d unanchored, want 0: %+v", len(unanchored), unanchored)
	}
	bodies := map[string]bool{}
	for _, c := range got {
		bodies[c.Body] = true
	}
	if !bodies["blocking: nil deref"] || !bodies["suggestion: extract helper"] {
		t.Fatalf("wrong survivors: %+v", got)
	}
}

// Dedupe runs after path normalisation, so clone-relative and repo-relative spellings of one finding collapse.
func TestValidCommentsDropsDuplicatesAcrossPathSpellings(t *testing.T) {
	app := newReviewApp(t, nil)
	in := []sdk.ReviewComment{
		{Path: "auth.go", Line: 42, Body: "blocking: nil deref"},
		{Path: "widgets/auth.go", Line: 42, Body: "blocking: nil deref"},
	}
	got, unanchored := app.validComments(context.Background(), "acme", "widgets", 7, in)
	if len(got) != 1 {
		t.Fatalf("validComments kept %d, want 1 (cross-spelling dup dropped): %+v", len(got), got)
	}
	if len(unanchored) != 0 {
		t.Fatalf("validComments left %d unanchored, want 0: %+v", len(unanchored), unanchored)
	}
}

// A delivered plan comment must not lead with a narration line or wrap the body in an outer ```markdown fence
// (GitHub would render it as one literal code block).
func TestSanitizeCommentBody(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "narration lead is dropped, the plan is kept",
			in:   "I need to fix the mermaid diagrams — let me replace them.\n\n## Plan\n\nDo the thing.",
			want: "## Plan\n\nDo the thing.",
		},
		{
			name: "second narration form",
			in:   "I've read all the relevant files. Here is the implementation plan.\n\n## Plan\n\nStep one.",
			want: "## Plan\n\nStep one.",
		},
		{
			name: "outer markdown fence is unwrapped",
			in:   "```markdown\n## Plan\n\nDo the thing.\n```",
			want: "## Plan\n\nDo the thing.",
		},
		{
			name: "outer md fence, case-insensitive",
			in:   "```MD\n## Plan\n```",
			want: "## Plan",
		},
		{
			name: "both defects together",
			in:   "```markdown\nLet me lay out the plan.\n\n## Plan\n\nDo it.\n```",
			want: "## Plan\n\nDo it.",
		},
		{
			name: "a legitimate inner code fence is left alone",
			in:   "## Plan\n\nRun:\n\n```go\nfmt.Println(\"hi\")\n```\n",
			want: "## Plan\n\nRun:\n\n```go\nfmt.Println(\"hi\")\n```",
		},
		{
			name: "clean body is untouched",
			in:   "## Plan\n\nDo the thing.",
			want: "## Plan\n\nDo the thing.",
		},
		{
			name: "narration-only body ships as-is rather than going empty",
			in:   "I'll just say hi.",
			want: "I'll just say hi.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.TrimSpace(sanitizeCommentBody(tt.in))
			if got != tt.want {
				t.Errorf("sanitizeCommentBody(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The posted comment (marker included) carries neither defect through the real deliverStagedComment path.
func TestDeliverStagedCommentSanitizesBody(t *testing.T) {
	var posted string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			io.WriteString(w, `[]`) // no prior quack comment for this slot
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			b, _ := io.ReadAll(r.Body)
			var body struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(b, &body)
			posted = body.Body
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	raw := "```markdown\nI've read the repo. Here is the plan.\n\n## Plan\n\nDo the thing.\n```"
	if err := app.deliverStagedComment(context.Background(), "acme", "widgets", 7, "plan", raw, "chat1"); err != nil {
		t.Fatalf("deliverStagedComment: %v", err)
	}
	if strings.Contains(posted, "```markdown") {
		t.Errorf("posted comment still carries the outer fence: %q", posted)
	}
	if strings.Contains(posted, "Here is the plan") {
		t.Errorf("posted comment still carries the narration lead: %q", posted)
	}
	if !strings.Contains(posted, "## Plan") || !strings.Contains(posted, "Do the thing.") {
		t.Errorf("posted comment lost the actual plan content: %q", posted)
	}
}

// An issue-scoped run that opens a PR and stages a review must review the PR it just created,
// not POST to pulls/<issue-number> and lose it.
func TestDeliverReviewTargetsCreatedPRNotIssue(t *testing.T) {
	const issueNum, createdPR = 61, 96
	var reviewPaths []string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls") && r.URL.Query().Get("head") != "":
			io.WriteString(w, `[]`) // no existing PR for the branch
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/acme/widgets/pull/%d"}`, createdPR, createdPR)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
			reviewPaths = append(reviewPaths, r.URL.Path)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/96#pullrequestreview-1"}`)
		default:
			io.WriteString(w, `{}`)
		}
	})
	dc := sdk.DeliveryContext{
		ChatID: "github-acme-widgets-61", IssueNumber: issueNum, CloneURL: "https://github.com/acme/widgets.git",
		Branch:     "feat/migrate", // PushedSHA empty, so Deliver skips push verification and goes straight to the items
		GatePassed: true,
		Items: []sdk.StagedDelivery{
			{Kind: "pull_request", Title: "migrate", Body: "body"},
			{Kind: "review", Event: "comment", Body: "findings"},
		},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(reviewPaths) != 1 {
		t.Fatalf("review submitted %d times, want 1: %v", len(reviewPaths), reviewPaths)
	}
	if want := fmt.Sprintf("/pulls/%d/reviews", createdPR); !strings.HasSuffix(reviewPaths[0], want) {
		t.Errorf("review posted to %q, want it to end %q - posting to the ISSUE number 404s and loses the review (#652)",
			reviewPaths[0], want)
	}
}

// A 422 on inline anchors must not lose the review: a mid-run push can leave findings off the current diff,
// so every finding moves to the summary.
func TestSubmitReviewMovesAllFindingsToSummaryOn422(t *testing.T) {
	var attempts int
	var lastBody string
	var lastComments int
	app := seededApp(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pulls/7/reviews") {
			t.Errorf("unexpected path %s", r.URL.Path)
			return
		}
		var req struct {
			Body     string `json:"body"`
			Comments []any  `json:"comments"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		attempts++
		lastBody, lastComments = req.Body, len(req.Comments)
		if attempts == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"Unprocessable Entity","errors":["Line could not be resolved"]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":99,"html_url":"https://github.com/acme/widgets/pull/7#r99"}`)
	})

	res, err := app.submitReview(context.Background(), submitReviewArgs{
		Owner: "acme", Repo: "widgets", PullNumber: 7, Event: "COMMENT", Body: "summary",
		Comments: []reviewComment{
			{Path: "auth.go", Line: 42, Body: "valid anchor"},
			{Path: "auth.go", Line: 999, Body: "stale anchor"},
		},
	})
	if err != nil {
		t.Fatalf("submitReview: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d; want 2 (inline, then summary-only)", attempts)
	}
	if res.ReviewID != 99 {
		t.Errorf("ReviewID = %d; want 99", res.ReviewID)
	}
	if lastComments != 0 {
		t.Errorf("retry sent %d inline comments; want 0", lastComments)
	}
	for _, want := range []string{"valid anchor", "stale anchor", "auth.go"} {
		if !strings.Contains(lastBody, want) {
			t.Errorf("summary lost %q: %s", want, lastBody)
		}
	}
	if res.Comments != 0 {
		t.Errorf("result reports %d inline comments; want 0", res.Comments)
	}
}

// TestPostingToolsHonorCallInfo: an /explain run (deny-all grant) or a
// read-only node must post nothing; an older host (no CallInfo) posts as before.
func TestPostingToolsHonorCallInfo(t *testing.T) {
	posts := 0
	app := seededApp(t, func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"html_url":"https://github.com/acme/widgets/pull/7#x"}`)
	})
	tools := map[string]func(context.Context) error{
		"github_comment": func(ctx context.Context) error {
			_, err := app.comment(ctx, commentArgs{Owner: "acme", Repo: "widgets", IssueNumber: 7, Body: "hi"})
			return err
		},
		"github_reply_to_review_comment": func(ctx context.Context) error {
			_, err := app.reply(ctx, replyArgs{Owner: "acme", Repo: "widgets", PullNumber: 7, CommentID: 11, Body: "hi"})
			return err
		},
		"github_react_to_comment": func(ctx context.Context) error {
			_, err := app.react(ctx, reactArgs{Owner: "acme", Repo: "widgets", CommentID: 11, CommentType: "issue_comment", Content: "eyes"})
			return err
		},
	}
	bg := context.Background()
	cases := []struct {
		name    string
		ctx     context.Context
		refusal string // "" = must post
	}{
		{"older host", bg, ""},
		{"unrestricted", sdk.WithCallInfo(bg, sdk.CallInfo{}), ""},
		{"comment granted", sdk.WithCallInfo(bg, sdk.CallInfo{AllowedDeliveryKinds: []sdk.DeliveryKind{sdk.KindReview, sdk.KindComment}}), ""},
		{"deny-all", sdk.WithCallInfo(bg, sdk.CallInfo{AllowedDeliveryKinds: []sdk.DeliveryKind{}}), "delivery grant"},
		{"review only", sdk.WithCallInfo(bg, sdk.CallInfo{AllowedDeliveryKinds: []sdk.DeliveryKind{sdk.KindReview}}), "delivery grant"},
		{"read-only", sdk.WithCallInfo(bg, sdk.CallInfo{ReadOnly: true}), "read-only"},
	}
	for name, call := range tools {
		for _, tc := range cases {
			before := posts
			err := call(tc.ctx)
			switch {
			case tc.refusal == "" && (err != nil || posts == before):
				t.Errorf("%s/%s: err %v, posts %d; want a post", name, tc.name, err, posts-before)
			case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal) || !strings.HasPrefix(err.Error(), name) || posts != before):
				t.Errorf("%s/%s: err %v, posts %d; want a %q refusal and no post", name, tc.name, err, posts-before, tc.refusal)
			}
		}
	}
}

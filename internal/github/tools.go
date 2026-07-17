package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// deliveryResults records each run's LAST commitDeliveryOnPass outcome, keyed
// by chat/session id (DeliveryContext.ChatID) — dispatch (webhook.go) reads
// it after drive() returns to tell "delivered" from "the gate passed but the
// push/post itself then failed", which the SSE stream alone can't (a judge
// pass means commitDeliveryOnPass RAN, not that it succeeded — see drive's
// doc comment). Process-local: one process serves one App instance, and a
// result is read (and cleared) exactly once, by the dispatch that caused it.
var deliveryResults sync.Map // chatID → error (nil = success)

func recordDeliveryResult(chatID string, err error) {
	if chatID == "" {
		return
	}
	deliveryResults.Store(chatID, deliveryOutcome{err})
}

// takeDeliveryResult returns and clears the last delivery outcome for chatID.
// ok is false when nothing was ever staged for this chat (dispatch then falls
// back to its own judge-pass proxy).
func takeDeliveryResult(chatID string) (err error, ok bool) {
	v, ok := deliveryResults.LoadAndDelete(chatID)
	if !ok {
		return nil, false
	}
	return v.(deliveryOutcome).err, true
}

// deliveryOutcome wraps a possibly-nil error so sync.Map can distinguish "no
// entry" (LoadAndDelete's own ok=false) from "entry present, err is nil".
type deliveryOutcome struct{ err error }

// gitHost is the only host this extension supplies credentials for.
const gitHost = "github.com"

// gitUsername is GitHub's recommended placeholder username for token auth
// (the token itself is the password) — mirrors the git-credential default.
const gitUsername = "x-access-token"

// GitCredential implements tools.GitTokenSource: for a github.com clone/remote
// URL it resolves the repo's installation and mints a fresh installation token,
// injected as the git credential. Returns (nil, nil) for any other host so the
// git op proceeds unauthenticated / falls back to a static credential.
func (a *App) GitCredential(ctx context.Context, rawURL string) (*tools.GitCredential, error) {
	owner, repo, ok := ownerRepoFromURL(rawURL)
	if !ok {
		return nil, nil
	}
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		// The App is not installed on this repo. That is NOT an error: a PUBLIC repo
		// clones fine with no credential at all. Attaching an installation token
		// scoped to the operator's own account is precisely what turns a public
		// clone into a 404 (live: OpenHands/goose/cloudflare all failed this way).
		// Return no credential and let git proceed anonymously.
		if errors.Is(err, ErrNoInstallation) {
			return nil, nil
		}
		return nil, fmt.Errorf("github: mint git credential for %s/%s: %w", owner, repo, err)
	}
	return &tools.GitCredential{Host: gitHost, Username: gitUsername, Token: tok}, nil
}

// ownerRepoFromURL extracts owner/repo from a github.com https URL, e.g.
// https://github.com/acme/widgets(.git) → ("acme","widgets"). ok is false for
// any non-github.com host or a path without both segments.
func ownerRepoFromURL(rawURL string) (owner, repo string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(u.Hostname(), gitHost) {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

// Tools are the extension's outbound capabilities, authed via the App's
// installation token. Kept minimal for the MVP; richer tools (create issue,
// request review, labels, check-runs) are documented follow-ups.
//
// NOT here (deliberately): pullRequestTool and submitReviewTool. Opening a PR
// or submitting a review makes work PUBLIC, so under the staged-delivery spine
// (0.5.0 — see internal/tools/stage_delivery.go, vetting.commitDeliveryOnPass)
// no agent calls them directly anymore — a worker STAGES that intent
// (stage_pr/stage_review) and the trust gate posts it, exactly once, only on a
// judge pass. createPullRequest/createReview (internal/github/app.go) are still
// here, called ONLY by the harness's own delivery step (internal/github/webhook.go).
func (a *App) Tools() []tool.Tool {
	return []tool.Tool{
		a.commentTool(),
		a.addReviewCommentTool(),
		a.listReviewCommentsTool(),
		a.deleteReviewCommentTool(),
		a.listPRCommentsTool(),
		a.replyToReviewCommentTool(),
		a.reactToCommentTool(),
	}
}

type commentArgs struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	IssueNumber int    `json:"issue_number"`
	Body        string `json:"body"`
}

type commentResult struct {
	Posted bool `json:"posted"`
}

func (a *App) commentTool() tool.Tool {
	t, _ := functiontool.New[commentArgs, commentResult](
		functiontool.Config{
			Name: "github_comment",
			Description: "Post a comment on a GitHub issue or pull request (PR conversation comments are " +
				"issue comments). `owner`/`repo` identify the repository, `issue_number` the issue/PR number, " +
				"`body` the markdown comment text. Authenticated as the app installation.",
		},
		func(ctx adkagent.Context, args commentArgs) (commentResult, error) {
			if args.Owner == "" || args.Repo == "" || args.IssueNumber == 0 || strings.TrimSpace(args.Body) == "" {
				return commentResult{}, fmt.Errorf("github_comment: owner, repo, issue_number and body are all required")
			}
			if err := a.postIssueComment(ctx, args.Owner, args.Repo, args.IssueNumber, args.Body); err != nil {
				return commentResult{}, err
			}
			return commentResult{Posted: true}, nil
		},
	)
	return t
}

// reviewComment is one inline comment on a PR review. It doubles as both the
// draft element and the GitHub reviews-API comment shape (identical JSON):
// path+line+body are required, side (default RIGHT) and start_line/start_side
// (for a multi-line range) are optional.
type reviewComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

// reviewEvents are the verdicts GitHub's reviews API accepts.
var reviewEvents = map[string]bool{"COMMENT": true, "REQUEST_CHANGES": true, "APPROVE": true}

// --- Review-draft CRUD tools ---
//
// A PR review is built up one inline comment at a time — each comment's inline
// LOCATION (path + line) is validated against the PR diff the moment it's added,
// so a bad line ref is caught with a clear, actionable error instead of sinking
// the whole review with a 422 at submit. The accumulated comments live in a
// process-local per-PR draft (App.drafts); github_submit_review posts them all
// as ONE review and clears the draft. Because every drafted comment was already
// location-validated, the final submit can't 422 on a bad line.

type addReviewCommentArgs struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	PullNumber int    `json:"pull_number"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Side       string `json:"side,omitempty"`
	StartLine  int    `json:"start_line,omitempty"`
	StartSide  string `json:"start_side,omitempty"`
	Body       string `json:"body"`
}

type addReviewCommentResult struct {
	Index      int `json:"index"`       // this comment's position in the draft
	DraftCount int `json:"draft_count"` // total comments now in the draft
}

func (a *App) addReviewCommentTool() tool.Tool {
	t, _ := functiontool.New[addReviewCommentArgs, addReviewCommentResult](
		functiontool.Config{
			Name: "github_add_review_comment",
			Description: "Record ONE inline comment on a pull request, anchored to `path` and `line` in the diff. " +
				"`path` is REPO-RELATIVE, exactly as the file appears in the PR diff (`app/game.ts`) — NOT the " +
				"workspace/clone path you read it from (`games/app/game.ts`); a clone-dir prefix is stripped for you " +
				"when it resolves to exactly one changed file. " +
				"The line is validated against the PR diff immediately: if `path` isn't a changed file or `line` " +
				"isn't a commentable line in its diff, the comment is REJECTED with the valid line range so you can " +
				"fix it. Accepted comments accumulate in a draft (they are NOT posted yet) — call github_submit_review " +
				"to post them all as one review. Optional `side` (LEFT/RIGHT, default RIGHT) and `start_line`+`start_side` " +
				"for a multi-line range. Record each finding here the moment you spot it; this draft is your durable " +
				"review memory. Authenticated as the app installation.",
		},
		func(ctx adkagent.Context, args addReviewCommentArgs) (addReviewCommentResult, error) {
			return a.addReviewComment(ctx, args)
		},
	)
	return t
}

// addReviewComment is the location-validating core of github_add_review_comment,
// split out so it's testable without an ADK context (the adk context satisfies
// context.Context, which is all the diff fetch needs).
func (a *App) addReviewComment(ctx context.Context, args addReviewCommentArgs) (addReviewCommentResult, error) {
	if args.Owner == "" || args.Repo == "" || args.PullNumber == 0 || args.Path == "" || args.Line == 0 || strings.TrimSpace(args.Body) == "" {
		return addReviewCommentResult{}, fmt.Errorf("github_add_review_comment: owner, repo, pull_number, path, line and body are all required")
	}
	side := strings.ToUpper(strings.TrimSpace(args.Side))
	if side == "" {
		side = "RIGHT"
	}
	if side != "LEFT" && side != "RIGHT" {
		return addReviewCommentResult{}, fmt.Errorf("github_add_review_comment: side must be LEFT or RIGHT; got %q", args.Side)
	}
	positions, err := a.commentablePositions(ctx, args.Owner, args.Repo, args.PullNumber)
	if err != nil {
		return addReviewCommentResult{}, err
	}
	path, err := resolvePath(positions, args.Path)
	if err != nil {
		return addReviewCommentResult{}, err
	}
	if err := validateLocation(positions, path, args.Line, side); err != nil {
		return addReviewCommentResult{}, err
	}
	if args.StartLine != 0 {
		if err := validateLocation(positions, path, args.StartLine, sideOr(args.StartSide, side)); err != nil {
			return addReviewCommentResult{}, fmt.Errorf("start_line: %w", err)
		}
	}
	c := reviewComment{Path: path, Line: args.Line, Body: args.Body}
	if side != "RIGHT" {
		c.Side = side
	}
	if args.StartLine != 0 {
		c.StartLine = args.StartLine
		c.StartSide = sideOr(args.StartSide, side)
	}
	idx := a.draftAdd(args.Owner, args.Repo, args.PullNumber, c)
	return addReviewCommentResult{Index: idx, DraftCount: idx + 1}, nil
}

// sideOr returns the trimmed upper-cased s, or fallback when s is empty.
func sideOr(s, fallback string) string {
	if u := strings.ToUpper(strings.TrimSpace(s)); u != "" {
		return u
	}
	return fallback
}

// resolvePath maps the agent's `path` onto a file in the PR diff. A PR diff
// addresses files REPO-relative ("app/game.ts"), but the agent works inside a
// clone directory in its workspace and naturally says "games/app/game.ts" — so
// on an inexact match we resolve by path-segment suffix (either direction).
// Exactly one candidate → accept it. Zero or many → an actionable error: the
// model cannot self-correct from "not a changed file" alone.
func resolvePath(positions map[string]diffPositions, path string) (string, error) {
	p := strings.Trim(strings.TrimPrefix(strings.TrimSpace(path), "./"), "/")
	if _, ok := positions[p]; ok {
		return p, nil
	}
	var candidates []string
	for f := range positions {
		if strings.HasSuffix(p, "/"+f) || strings.HasSuffix(f, "/"+p) {
			candidates = append(candidates, f)
		}
	}
	sort.Strings(candidates)
	switch len(candidates) {
	case 1:
		slog.Debug("github: normalised review-comment path to its repo-relative form",
			"component", "github", "given", path, "resolved", candidates[0])
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("github_add_review_comment: %q is not a changed file in this PR. `path` must be REPO-RELATIVE, exactly as the file appears in the PR diff (e.g. \"app/game.ts\"), NOT the workspace/clone path (e.g. \"games/app/game.ts\"). Changed files in this PR: %s", path, joinCapped(changedFiles(positions)))
	default:
		return "", fmt.Errorf("github_add_review_comment: %q is ambiguous — it matches several changed files: %s. Re-send `path` as the full repo-relative path of the one you mean", path, joinCapped(candidates))
	}
}

// changedFiles returns the PR's changed files, sorted.
func changedFiles(positions map[string]diffPositions) []string {
	files := make([]string, 0, len(positions))
	for f := range positions {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// joinCapped renders paths as a comma-separated list, capped so a huge PR can't
// blow the model's context.
func joinCapped(paths []string) string {
	if len(paths) == 0 {
		return "(none — this PR has no changed files with a diff)"
	}
	const maxShown = 30
	if len(paths) > maxShown {
		return strings.Join(paths[:maxShown], ", ") + fmt.Sprintf(", … (%d more)", len(paths)-maxShown)
	}
	return strings.Join(paths, ", ")
}

// validateLocation checks line is commentable on the given side of path (already
// resolved to a changed file by resolvePath), returning a clear error (with the
// valid line range) otherwise.
func validateLocation(positions map[string]diffPositions, path string, line int, side string) error {
	dp, ok := positions[path]
	if !ok {
		return fmt.Errorf("github_add_review_comment: %q is not a changed file in this PR — inline comments must target a file in the diff", path)
	}
	lines := dp.right
	if side == "LEFT" {
		lines = dp.left
	}
	if !lines[line] {
		return fmt.Errorf("github_add_review_comment: line %d is not commentable on the %s side of %q; commentable lines: %s", line, side, path, describeLines(lines))
	}
	return nil
}

// describeLines renders a compact, sorted summary of commentable line numbers
// (capped) for an actionable error message.
func describeLines(lines map[int]bool) string {
	if len(lines) == 0 {
		return "(none — file has no commentable lines)"
	}
	nums := make([]int, 0, len(lines))
	for n := range lines {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	const maxShown = 30
	if len(nums) > maxShown {
		parts := make([]string, maxShown)
		for i := 0; i < maxShown; i++ {
			parts[i] = strconv.Itoa(nums[i])
		}
		return strings.Join(parts, ", ") + fmt.Sprintf(", … (%d more)", len(nums)-maxShown)
	}
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

type draftPRArgs struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	PullNumber int    `json:"pull_number"`
}

type draftedComment struct {
	Index int `json:"index"`
	reviewComment
}

type listReviewCommentsResult struct {
	Comments []draftedComment `json:"comments"`
}

func (a *App) listReviewCommentsTool() tool.Tool {
	t, _ := functiontool.New[draftPRArgs, listReviewCommentsResult](
		functiontool.Config{
			Name: "github_list_review_comments",
			Description: "List the inline review comments you've recorded so far for a pull request but not yet " +
				"submitted (the pending draft), each with its `index` for github_delete_review_comment. Use this to " +
				"see everything you've captured before submitting. `owner`/`repo`/`pull_number` identify the PR.",
		},
		func(_ adkagent.Context, args draftPRArgs) (listReviewCommentsResult, error) {
			return a.listReviewComments(args)
		},
	)
	return t
}

func (a *App) listReviewComments(args draftPRArgs) (listReviewCommentsResult, error) {
	if args.Owner == "" || args.Repo == "" || args.PullNumber == 0 {
		return listReviewCommentsResult{}, fmt.Errorf("github_list_review_comments: owner, repo and pull_number are all required")
	}
	draft := a.draftList(args.Owner, args.Repo, args.PullNumber)
	out := make([]draftedComment, len(draft))
	for i, c := range draft {
		out[i] = draftedComment{Index: i, reviewComment: c}
	}
	return listReviewCommentsResult{Comments: out}, nil
}

type deleteReviewCommentArgs struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	PullNumber int    `json:"pull_number"`
	Index      int    `json:"index"`
}

type deleteReviewCommentResult struct {
	Deleted    bool `json:"deleted"`
	DraftCount int  `json:"draft_count"`
}

func (a *App) deleteReviewCommentTool() tool.Tool {
	t, _ := functiontool.New[deleteReviewCommentArgs, deleteReviewCommentResult](
		functiontool.Config{
			Name: "github_delete_review_comment",
			Description: "Remove one inline comment from a pull request's pending review draft by its `index` (from " +
				"github_list_review_comments). To edit a comment, delete it and add a corrected one. Note: after a " +
				"delete the remaining comments' indices shift down — re-list to get current indices. " +
				"`owner`/`repo`/`pull_number` identify the PR.",
		},
		func(_ adkagent.Context, args deleteReviewCommentArgs) (deleteReviewCommentResult, error) {
			return a.deleteReviewComment(args)
		},
	)
	return t
}

func (a *App) deleteReviewComment(args deleteReviewCommentArgs) (deleteReviewCommentResult, error) {
	if args.Owner == "" || args.Repo == "" || args.PullNumber == 0 {
		return deleteReviewCommentResult{}, fmt.Errorf("github_delete_review_comment: owner, repo and pull_number are all required")
	}
	if !a.draftDelete(args.Owner, args.Repo, args.PullNumber, args.Index) {
		return deleteReviewCommentResult{}, fmt.Errorf("github_delete_review_comment: no draft comment at index %d", args.Index)
	}
	return deleteReviewCommentResult{Deleted: true, DraftCount: len(a.draftList(args.Owner, args.Repo, args.PullNumber))}, nil
}

type submitReviewArgs struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	PullNumber int    `json:"pull_number"`
	Body       string `json:"body,omitempty"`
	Event      string `json:"event"`
}

type submitReviewResult struct {
	URL      string `json:"url"`
	ReviewID int64  `json:"review_id"`
	Comments int    `json:"comments"`
}

// submitReview is the review-draft submit itself, kept for the delivery step
// (internal/github/webhook.go's commitDelivery, called only post-judge-pass) —
// it is no longer exposed as a model tool (see App.Tools).
func (a *App) submitReview(ctx context.Context, args submitReviewArgs) (submitReviewResult, error) {
	if args.Owner == "" || args.Repo == "" || args.PullNumber == 0 {
		return submitReviewResult{}, fmt.Errorf("github_submit_review: owner, repo and pull_number are all required")
	}
	event := strings.ToUpper(strings.TrimSpace(args.Event))
	if !reviewEvents[event] {
		return submitReviewResult{}, fmt.Errorf("github_submit_review: event must be one of COMMENT, REQUEST_CHANGES, APPROVE; got %q", args.Event)
	}
	comments := a.draftList(args.Owner, args.Repo, args.PullNumber)
	body := strings.TrimSpace(args.Body)
	if body == "" {
		// Both the mid-tier and the coder models sometimes submit an empty body.
		// Never post a review with no summary — synthesise a minimal takeaway from
		// the verdict and the inline count so the PR always shows one.
		body = defaultReviewBody(event, len(comments))
	}
	url, id, err := a.createReview(ctx, args.Owner, args.Repo, args.PullNumber, event, body, comments)
	if err != nil {
		return submitReviewResult{}, err
	}
	a.draftTake(args.Owner, args.Repo, args.PullNumber) // clear only after a successful post
	return submitReviewResult{URL: url, ReviewID: id, Comments: len(comments)}, nil
}

// defaultReviewBody synthesises a one-line summary for a review submitted with an
// empty body — a verdict word plus the inline-comment count, so the PR never shows
// a blank review summary.
func defaultReviewBody(event string, n int) string {
	verdict := "Reviewed"
	switch event {
	case "REQUEST_CHANGES":
		verdict = "Requested changes"
	case "APPROVE":
		verdict = "Approved"
	}
	switch n {
	case 0:
		return verdict + "."
	case 1:
		return verdict + " — 1 inline comment, see it for detail."
	default:
		return fmt.Sprintf("%s — %d inline comments, see them for detail.", verdict, n)
	}
}

// --- Reading & reacting to existing PR discussion ---

func (a *App) listPRCommentsTool() tool.Tool {
	t, _ := functiontool.New[draftPRArgs, prDiscussion](
		functiontool.Config{
			Name: "github_list_pr_comments",
			Description: "Read a pull request's EXISTING discussion before you add your own review, so you don't " +
				"repeat what's already been said: its inline review comments (path/line/body/user, with in_reply_to_id " +
				"for threads), its top-level conversation comments, and its submitted reviews (body/state/user). " +
				"`owner`/`repo`/`pull_number` identify the PR. Authenticated as the app installation.",
		},
		func(ctx adkagent.Context, args draftPRArgs) (prDiscussion, error) {
			if args.Owner == "" || args.Repo == "" || args.PullNumber == 0 {
				return prDiscussion{}, fmt.Errorf("github_list_pr_comments: owner, repo and pull_number are all required")
			}
			return a.listPRDiscussion(ctx, args.Owner, args.Repo, args.PullNumber)
		},
	)
	return t
}

type replyArgs struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	PullNumber int    `json:"pull_number"`
	CommentID  int64  `json:"comment_id"`
	Body       string `json:"body"`
}

type replyResult struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

func (a *App) replyToReviewCommentTool() tool.Tool {
	t, _ := functiontool.New[replyArgs, replyResult](
		functiontool.Config{
			Name: "github_reply_to_review_comment",
			Description: "Reply in-thread to an existing inline review comment (acknowledge, agree, add context) " +
				"instead of opening a new thread. `comment_id` is the review comment you're replying to (from " +
				"github_list_pr_comments). `owner`/`repo`/`pull_number` identify the PR, `body` is the reply text. " +
				"Authenticated as the app installation.",
		},
		func(ctx adkagent.Context, args replyArgs) (replyResult, error) {
			return a.reply(ctx, args)
		},
	)
	return t
}

func (a *App) reply(ctx context.Context, args replyArgs) (replyResult, error) {
	if args.Owner == "" || args.Repo == "" || args.PullNumber == 0 || args.CommentID == 0 || strings.TrimSpace(args.Body) == "" {
		return replyResult{}, fmt.Errorf("github_reply_to_review_comment: owner, repo, pull_number, comment_id and body are all required")
	}
	id, url, err := a.replyToReviewComment(ctx, args.Owner, args.Repo, args.PullNumber, args.CommentID, args.Body)
	if err != nil {
		return replyResult{}, err
	}
	return replyResult{ID: id, URL: url}, nil
}

// reactionContents are the emoji reactions GitHub accepts.
var reactionContents = map[string]bool{
	"+1": true, "-1": true, "laugh": true, "hooray": true, "confused": true, "heart": true, "rocket": true, "eyes": true,
}

// commentTypePaths maps a comment_type to the reactions endpoint's comment family.
var commentTypePaths = map[string]string{"review_comment": "pulls", "issue_comment": "issues"}

type reactArgs struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	CommentID   int64  `json:"comment_id"`
	CommentType string `json:"comment_type"`
	Content     string `json:"content"`
}

type reactResult struct {
	ReactionID int64 `json:"reaction_id"`
}

func (a *App) reactToCommentTool() tool.Tool {
	t, _ := functiontool.New[reactArgs, reactResult](
		functiontool.Config{
			Name: "github_react_to_comment",
			Description: "Add an emoji reaction to a comment — a lightweight acknowledgment. `comment_id` is the " +
				"comment; `comment_type` is `review_comment` (an inline review comment) or `issue_comment` (a " +
				"conversation comment); `content` is one of +1, -1, laugh, hooray, confused, heart, rocket, eyes. " +
				"`owner`/`repo` identify the repo. Authenticated as the app installation.",
		},
		func(ctx adkagent.Context, args reactArgs) (reactResult, error) {
			return a.react(ctx, args)
		},
	)
	return t
}

func (a *App) react(ctx context.Context, args reactArgs) (reactResult, error) {
	if args.Owner == "" || args.Repo == "" || args.CommentID == 0 {
		return reactResult{}, fmt.Errorf("github_react_to_comment: owner, repo and comment_id are all required")
	}
	commentPath, ok := commentTypePaths[args.CommentType]
	if !ok {
		return reactResult{}, fmt.Errorf("github_react_to_comment: comment_type must be review_comment or issue_comment; got %q", args.CommentType)
	}
	if !reactionContents[args.Content] {
		return reactResult{}, fmt.Errorf("github_react_to_comment: content must be one of +1, -1, laugh, hooray, confused, heart, rocket, eyes; got %q", args.Content)
	}
	id, err := a.reactToComment(ctx, args.Owner, args.Repo, commentPath, args.CommentID, args.Content)
	if err != nil {
		return reactResult{}, err
	}
	return reactResult{ReactionID: id}, nil
}

// openPullRequest opens a PR and best-effort applies labels — the delivery
// step's own call (internal/github/webhook.go's commitDelivery, post-judge-
// pass only); no longer exposed as a model tool (see App.Tools). A label
// failure never fails the open (a retry would duplicate the PR).
func (a *App) openPullRequest(ctx context.Context, owner, repo, title, head, base, body string, labels []string) (string, error) {
	if base == "" {
		base = "main"
	}
	u, number, err := a.createPullRequest(ctx, owner, repo, title, head, base, body)
	if err != nil {
		return "", err
	}
	if len(labels) > 0 {
		if err := a.addLabels(ctx, owner, repo, number, labels); err != nil {
			slog.Warn("github: labeling the new PR failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}
	return u, nil
}

// Deliver is the vetting.DeliverFunc this extension provides (wired in
// internal/serve): the ONE place, this whole extension, that pushes a branch
// or posts anything to a triggering repo — called by commitDeliveryOnPass
// exactly once, only after a node's judge pass. It pushes dc.Branch
// (transient, App-authed — see tools.PushBranch), then works the staged set in
// order: opening a pull request first (so a staged review/comment on the SAME
// run has something fresh to land on), then submitting the review, then
// posting each comment. A later item's failure doesn't undo an earlier one's
// success; every failure is collected and returned together so the caller's
// one log line names all of them.
//
// jailRoot anchors the askpass symlink PushBranch needs (workspace.Jail.Root()).
func (a *App) Deliver(ctx context.Context, jailRoot string, dc vetting.DeliveryContext) (err error) {
	defer func() { recordDeliveryResult(dc.ChatID, err) }()
	if len(dc.Items) == 0 {
		return nil
	}
	owner, repo, ok := ownerRepoFromURL(dc.CloneURL)
	if !ok {
		return fmt.Errorf("github: delivery: %q is not a github.com clone URL — nothing to deliver against", dc.CloneURL)
	}
	if dc.Branch != "" && dc.CloneDir != "" {
		tok, terr := a.tokenForRepo(ctx, owner, repo)
		if terr != nil {
			return fmt.Errorf("github: delivery: %w", terr)
		}
		cred := tools.GitCredential{Host: gitHost, Username: gitUsername, Token: tok}
		if _, perr := tools.PushBranch(ctx, jailRoot, dc.CloneDir, dc.Branch, cred, workspace.DefaultCaps()); perr != nil {
			return fmt.Errorf("github: delivery: push %q: %w", dc.Branch, perr)
		}
	}
	var errs []error
	for _, item := range dc.Items {
		if ierr := a.deliverOne(ctx, owner, repo, dc, item); ierr != nil {
			errs = append(errs, ierr)
		}
	}
	return errors.Join(errs...)
}

// deliverOne posts one staged item, past Deliver's push. A review or comment
// with no known PR (dc.IssueNumber == 0 — the worker never named one via
// github_add_review_comment/github_submit_review) is a clear, actionable
// error rather than a guess.
func (a *App) deliverOne(ctx context.Context, owner, repo string, dc vetting.DeliveryContext, item vetting.StagedDelivery) error {
	switch item.Kind {
	case "pull_request":
		if dc.Branch == "" {
			return fmt.Errorf("github: delivery: staged pull request %q has no branch to open it from", item.Title)
		}
		u, err := a.openPullRequest(ctx, owner, repo, item.Title, dc.Branch, "", item.Body, nil)
		if err != nil {
			return fmt.Errorf("github: delivery: open pull request: %w", err)
		}
		slog.Info("github: delivered a pull request", "component", "github", "repo", owner+"/"+repo, "url", u)
		return nil
	case "review":
		if dc.IssueNumber == 0 {
			return fmt.Errorf("github: delivery: staged review has no pull request number to submit against")
		}
		event := strings.ToUpper(item.Event)
		if !reviewEvents[event] {
			return fmt.Errorf("github: delivery: staged review event %q is not one of approve/request_changes/comment", item.Event)
		}
		res, err := a.submitReview(ctx, submitReviewArgs{Owner: owner, Repo: repo, PullNumber: dc.IssueNumber, Body: item.Body, Event: event})
		if err != nil {
			return fmt.Errorf("github: delivery: submit review: %w", err)
		}
		slog.Info("github: delivered a review", "component", "github", "repo", owner+"/"+repo, "url", res.URL)
		return nil
	case "comment":
		if dc.IssueNumber == 0 {
			return fmt.Errorf("github: delivery: staged comment %q has no issue/PR number to post to", item.Slot)
		}
		if err := a.postIssueComment(ctx, owner, repo, dc.IssueNumber, item.Body); err != nil {
			return fmt.Errorf("github: delivery: post comment %q: %w", item.Slot, err)
		}
		return nil
	default:
		return fmt.Errorf("github: delivery: unknown staged kind %q", item.Kind)
	}
}

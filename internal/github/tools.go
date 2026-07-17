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
var deliveryResults sync.Map // chatID → deliveryOutcome

func recordDeliveryResult(chatID string, err error) {
	recordDelivery(chatID, deliveryOutcome{err: err})
}

// recordDelivery is recordDeliveryResult plus the verified GitHub state a
// successful delivery produced (real PR number/url, the pushed SHA) — so a
// caller reads what GitHub actually has, not what the worker claimed (T3.4).
func recordDelivery(chatID string, o deliveryOutcome) {
	if chatID == "" {
		return
	}
	deliveryResults.Store(chatID, o)
}

// takeDeliveryDetail returns and clears the last delivery outcome for chatID
// — its error, plus (on success) the verified PR/SHA info (T3.4). ok is false
// when nothing was ever staged for this chat (dispatch then falls back to its
// own judge-pass proxy).
func takeDeliveryDetail(chatID string) (deliveryOutcome, bool) {
	v, ok := deliveryResults.LoadAndDelete(chatID)
	if !ok {
		return deliveryOutcome{}, false
	}
	return v.(deliveryOutcome), true
}

// deliveryOutcome wraps a possibly-nil error (so sync.Map can distinguish "no
// entry" from "entry present, err is nil") plus, on a successful pull-request
// delivery, the real GitHub state it produced.
type deliveryOutcome struct {
	err       error
	prNumber  int
	prURL     string
	pushedSHA string
}

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
	// The marker makes a later run's collapse (T4.1) able to find this review
	// again; it never fails silently into a defaultReviewBody-free blank post.
	body += "\n\n" + deliveryMarker("review")
	url, id, err := a.createReview(ctx, args.Owner, args.Repo, args.PullNumber, event, body, comments)
	if err != nil {
		return submitReviewResult{}, err
	}
	a.draftTake(args.Owner, args.Repo, args.PullNumber) // clear only after a successful post
	return submitReviewResult{URL: url, ReviewID: id, Comments: len(comments)}, nil
}

// deliveryMarker is the hidden HTML-comment marker embedded in a quack-
// authored comment/review, so a later run can find its own prior post — to
// edit it in place (comment idempotency, T3.3) or collapse it as superseded
// (T4.1) — without touching a human's discussion. family is "plan", "review",
// or "comment:<slot>" (mirrors the staged-delivery target keys in node.go).
func deliveryMarker(family string) string {
	return "<!-- quack:delivery:" + family + " -->"
}

// collapsePriorReviews minimizes every existing quack-authored PR review
// (GraphQL minimizeComment, classifier OUTDATED) before a new one is
// submitted for the same PR, so the thread shows the CURRENT review, not a
// pile of dead attempts (T4.1). Best-effort: a failure is logged, never fails
// the new review's delivery.
func (a *App) collapsePriorReviews(ctx context.Context, owner, repo string, number int) {
	reviews, err := a.listReviews(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: collapse: list reviews failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	bot, err := a.botLogin(ctx)
	if err != nil {
		slog.Warn("github: collapse: bot identity lookup failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	marker := deliveryMarker("review")
	for _, r := range reviews {
		if r.User.Login != bot || r.NodeID == "" || !strings.Contains(r.Body, marker) {
			continue
		}
		if err := a.minimizeComment(ctx, owner, repo, r.NodeID); err != nil {
			slog.Warn("github: collapse review failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}
}

// collapsePriorComments minimizes every existing quack-authored issue comment
// carrying marker family (T4.1) — same idea as collapsePriorReviews, for
// comments rather than PR reviews (e.g. a superseded plan comment). Best-effort.
func (a *App) collapsePriorComments(ctx context.Context, owner, repo string, number int, family string) {
	comments, err := a.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: collapse: list comments failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		return
	}
	bot, err := a.botLogin(ctx)
	if err != nil {
		slog.Warn("github: collapse: bot identity lookup failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		return
	}
	marker := deliveryMarker(family)
	for _, c := range comments {
		if c.User != bot || c.NodeID == "" || !strings.Contains(c.Body, marker) {
			continue
		}
		if err := a.minimizeComment(ctx, owner, repo, c.NodeID); err != nil {
			slog.Warn("github: collapse comment failed", "component", "github", "repo", owner+"/"+repo, "issue", number, "err", err)
		}
	}
}

// findQuackComment returns the ID of an existing quack-authored issue comment
// carrying marker, if any — the revise-before-post lookup for T3.3. ok is
// false when none is found; err explains a lookup failure (the caller then
// falls back to posting fresh rather than risk never posting at all).
func (a *App) findQuackComment(ctx context.Context, owner, repo string, number int, marker string) (id int64, ok bool, err error) {
	comments, err := a.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return 0, false, err
	}
	bot, err := a.botLogin(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, c := range comments {
		if c.User == bot && strings.Contains(c.Body, marker) {
			return c.ID, true, nil
		}
	}
	return 0, false, nil
}

// deliverStagedComment posts (or, if a prior quack comment for this SAME slot
// already exists, edits) a staged comment — the marker makes a revise-before-
// post re-run idempotent instead of piling up duplicates (T3.3).
func (a *App) deliverStagedComment(ctx context.Context, owner, repo string, number int, slot, bodyText string) error {
	marker := deliveryMarker("comment:" + slot)
	withMarker := strings.TrimSpace(bodyText) + "\n\n" + marker
	id, found, err := a.findQuackComment(ctx, owner, repo, number, marker)
	if err != nil {
		slog.Warn("github: find prior comment failed; posting fresh", "component", "github", "repo", owner+"/"+repo, "issue", number, "slot", slot, "err", err)
	}
	if found {
		return a.editIssueComment(ctx, owner, repo, id, withMarker)
	}
	return a.postIssueComment(ctx, owner, repo, number, withMarker)
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
func (a *App) openPullRequest(ctx context.Context, owner, repo, title, head, base, body string, labels []string) (string, int, error) {
	if base == "" {
		base = "main"
	}
	u, number, err := a.createPullRequest(ctx, owner, repo, title, head, base, body)
	if err != nil {
		return "", 0, err
	}
	if len(labels) > 0 {
		if err := a.addLabels(ctx, owner, repo, number, labels); err != nil {
			slog.Warn("github: labeling the new PR failed", "component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		}
	}
	return u, number, nil
}

// openOrUpdatePullRequest is the idempotent PR delivery (T3.1): it opens a NEW
// pull request for head only when GitHub has no OPEN one already; otherwise it
// UPDATES the existing PR's title/body — "revise on re-run" instead of a
// second PR. Labels are applied only on the first open (an update never risks
// re-labeling). A failed existence check degrades to "open a new one" rather
// than blocking delivery outright — GitHub itself still rejects a genuine
// duplicate branch-to-PR mapping.
func (a *App) openOrUpdatePullRequest(ctx context.Context, owner, repo, title, head, base, body string, labels []string) (url string, number int, err error) {
	if num, _, ok, ferr := a.findOpenPR(ctx, owner, repo, head); ferr != nil {
		slog.Warn("github: check for an existing open PR failed; opening a new one", "component", "github", "repo", owner+"/"+repo, "branch", head, "err", ferr)
	} else if ok {
		u, uerr := a.updatePullRequest(ctx, owner, repo, num, title, body)
		if uerr != nil {
			return "", 0, fmt.Errorf("update existing pull request #%d: %w", num, uerr)
		}
		slog.Info("github: updated the existing open pull request instead of opening a duplicate",
			"component", "github", "repo", owner+"/"+repo, "pr", num, "url", u)
		return u, num, nil
	}
	return a.openPullRequest(ctx, owner, repo, title, head, base, body, labels)
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
	var detail deliveryOutcome
	defer func() {
		detail.err = err
		recordDelivery(dc.ChatID, detail)
	}()
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
		localSHA, perr := tools.PushBranch(ctx, jailRoot, dc.CloneDir, dc.Branch, cred, workspace.DefaultCaps())
		if perr != nil {
			return fmt.Errorf("github: delivery: push %q: %w", dc.Branch, perr)
		}
		// A `git push` that exits 0 is not proof the branch landed (a dropped
		// connection mid-push, a revoked installation) — confirm against GitHub's
		// OWN state before claiming anything downstream (T3.2). localSHA is a
		// short hash; GitHub's ref API returns the full one.
		remoteSHA, verr := a.branchHeadSHA(ctx, owner, repo, dc.Branch)
		if verr != nil {
			return fmt.Errorf("github: delivery: push %q: verify against GitHub: %w", dc.Branch, verr)
		}
		if !strings.HasPrefix(remoteSHA, localSHA) {
			return fmt.Errorf("github: delivery: push %q: local head %s not reflected on GitHub (remote head %s) — not delivering", dc.Branch, localSHA, remoteSHA)
		}
		detail.pushedSHA = remoteSHA
	}
	var errs []error
	for _, item := range dc.Items {
		res, ierr := a.deliverOne(ctx, owner, repo, dc, item)
		if ierr != nil {
			errs = append(errs, ierr)
			continue
		}
		if res.prNumber != 0 {
			detail.prNumber, detail.prURL = res.prNumber, res.prURL
		}
	}
	return errors.Join(errs...)
}

// deliveryItemResult is what one staged item's delivery produced worth
// recording — currently only a pull request's real number/url (T3.4).
type deliveryItemResult struct {
	prNumber int
	prURL    string
}

// deliverOne posts one staged item, past Deliver's push. A review or comment
// with no known PR (dc.IssueNumber == 0 — the worker never named one via
// github_add_review_comment/github_submit_review) is a clear, actionable
// error rather than a guess.
func (a *App) deliverOne(ctx context.Context, owner, repo string, dc vetting.DeliveryContext, item vetting.StagedDelivery) (deliveryItemResult, error) {
	switch item.Kind {
	case "pull_request":
		if dc.Branch == "" {
			return deliveryItemResult{}, fmt.Errorf("github: delivery: staged pull request %q has no branch to open it from", item.Title)
		}
		u, num, err := a.openOrUpdatePullRequest(ctx, owner, repo, item.Title, dc.Branch, "", item.Body, nil)
		if err != nil {
			return deliveryItemResult{}, fmt.Errorf("github: delivery: open pull request: %w", err)
		}
		slog.Info("github: delivered a pull request", "component", "github", "repo", owner+"/"+repo, "pr", num, "url", u)
		return deliveryItemResult{prNumber: num, prURL: u}, nil
	case "review":
		if dc.IssueNumber == 0 {
			return deliveryItemResult{}, fmt.Errorf("github: delivery: staged review has no pull request number to submit against")
		}
		event := strings.ToUpper(item.Event)
		if !reviewEvents[event] {
			return deliveryItemResult{}, fmt.Errorf("github: delivery: staged review event %q is not one of approve/request_changes/comment", item.Event)
		}
		a.collapsePriorReviews(ctx, owner, repo, dc.IssueNumber) // superseded prior attempts (T4.1)
		res, err := a.submitReview(ctx, submitReviewArgs{Owner: owner, Repo: repo, PullNumber: dc.IssueNumber, Body: item.Body, Event: event})
		if err != nil {
			return deliveryItemResult{}, fmt.Errorf("github: delivery: submit review: %w", err)
		}
		slog.Info("github: delivered a review", "component", "github", "repo", owner+"/"+repo, "url", res.URL)
		return deliveryItemResult{}, nil
	case "comment":
		if dc.IssueNumber == 0 {
			return deliveryItemResult{}, fmt.Errorf("github: delivery: staged comment %q has no issue/PR number to post to", item.Slot)
		}
		if err := a.deliverStagedComment(ctx, owner, repo, dc.IssueNumber, item.Slot, item.Body); err != nil {
			return deliveryItemResult{}, fmt.Errorf("github: delivery: post comment %q: %w", item.Slot, err)
		}
		return deliveryItemResult{}, nil
	default:
		return deliveryItemResult{}, fmt.Errorf("github: delivery: unknown staged kind %q", item.Kind)
	}
}

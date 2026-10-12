package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// deliveryResults records each run's last Deliver outcome by global chatID; process-local,
// read-and-cleared once by RunEnded.
var deliveryResults sync.Map // chatID → deliveryOutcome

// recordDelivery includes the verified GitHub state (PR number/url, pushed SHA).
func recordDelivery(chatID string, o deliveryOutcome) {
	if chatID == "" {
		return
	}
	deliveryResults.Store(chatID, o)
}

// takeDeliveryDetail returns and clears the last delivery outcome for chatID.
func takeDeliveryDetail(chatID string) (deliveryOutcome, bool) {
	v, ok := deliveryResults.LoadAndDelete(chatID)
	if !ok {
		return deliveryOutcome{}, false
	}
	return v.(deliveryOutcome), true
}

// deliveryOutcome wraps a possibly-nil error and the GitHub state a successful delivery produced.
type deliveryOutcome struct {
	err       error
	branch    string // for a failure comment, so the work is recoverable by hand
	prNumber  int
	prURL     string
	pushedSHA string
	// reviewDelivered is dispatch's only trigger to advance the review baseline.
	reviewDelivered bool
}

// gitHost is the only host this extension supplies credentials for.
const gitHost = "github.com"

// gitUsername is GitHub's recommended placeholder username for token auth.
const gitUsername = "x-access-token"

// GitCredential resolves the repo's installation and mints a token. Returns (nil, nil) for non-github.com hosts.
func (a *App) GitCredential(ctx context.Context, rawURL string) (*sdk.GitCredential, error) {
	owner, repo, ok := ownerRepoFromURL(rawURL)
	if !ok {
		return nil, nil
	}
	tok, err := a.tokenForRepo(ctx, owner, repo)
	if err != nil {
		// App not installed — return no credential so public repos clone anonymously.
		if errors.Is(err, ErrNoInstallation) {
			return nil, nil
		}
		return nil, fmt.Errorf("github: mint git credential for %s/%s: %w", owner, repo, err)
	}
	return &sdk.GitCredential{Host: gitHost, Username: gitUsername, Token: tok}, nil
}

// ownerRepoFromURL extracts owner/repo from a github.com URL like https://github.com/acme/widgets(.git).
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

// Tools are the extension's outbound capabilities (comment, reply, react).
// PR/review submission is gate-owned, never a model tool.
func (a *App) Tools() []tool.Tool {
	return []tool.Tool{
		a.commentTool(),
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
	return newTool[commentArgs, commentResult]("github_comment",
		"Post a comment on a GitHub issue or pull request (PR conversation comments are "+
			"issue comments). `owner`/`repo` identify the repository, `issue_number` the issue/PR number, "+
			"`body` the markdown comment text. Authenticated as the app installation.",
		func(ctx adkagent.Context, args commentArgs) (commentResult, error) { return a.comment(ctx, args) })
}

func (a *App) comment(ctx context.Context, args commentArgs) (commentResult, error) {
	if err := postRefusal(ctx, "github_comment"); err != nil {
		return commentResult{}, err
	}
	if args.Owner == "" || args.Repo == "" || args.IssueNumber == 0 || strings.TrimSpace(args.Body) == "" {
		return commentResult{}, fmt.Errorf("github_comment: owner, repo, issue_number and body are all required")
	}
	if err := a.postIssueComment(ctx, args.Owner, args.Repo, args.IssueNumber, args.Body); err != nil {
		return commentResult{}, err
	}
	return commentResult{Posted: true}, nil
}

// postRefusal says why this run may not post to GitHub, or nil. Replies and
// reactions count as "comment": on a PR, review always implies comment.
func postRefusal(ctx context.Context, name string) error {
	ci, ok := sdk.CallInfoFrom(ctx)
	switch {
	case !ok:
		return nil
	case ci.ReadOnly:
		return fmt.Errorf("%s: refused, this run is read-only and may not post to GitHub; put the text in your answer instead", name)
	case ci.AllowedDeliveryKinds != nil && !slices.Contains(ci.AllowedDeliveryKinds, sdk.KindComment):
		return fmt.Errorf("%s: refused, this run's delivery grant does not include %q, so it may not post to GitHub; put the text in your answer instead", name, sdk.KindComment)
	}
	return nil
}

// reviewComment is one inline PR review comment. Matches GitHub's reviews-API shape.
type reviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// reviewEvents are the verdicts GitHub's reviews API accepts.
var reviewEvents = map[string]bool{"COMMENT": true, "REQUEST_CHANGES": true, "APPROVE": true}

// resolvePath maps the agent's workspace-relative path to the PR diff's repo-relative path by suffix matching.
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
	slices.Sort(candidates)
	switch len(candidates) {
	case 1:
		slog.Debug("github: normalised review-comment path to its repo-relative form",
			"component", "github", "given", path, "resolved", candidates[0])
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("github_add_review_comment: %q is not a changed file in this PR. `path` must be REPO-RELATIVE, exactly as the file appears in the PR diff (e.g. \"app/game.ts\"), NOT the workspace/clone path (e.g. \"games/app/game.ts\"). Changed files in this PR: %s", path, joinCapped(changedFiles(positions)))
	default:
		return "", fmt.Errorf("github_add_review_comment: %q is ambiguous - it matches several changed files: %s. Re-send `path` as the full repo-relative path of the one you mean", path, joinCapped(candidates))
	}
}

// changedFiles returns the PR's changed files, sorted.
func changedFiles(positions map[string]diffPositions) []string {
	return slices.Sorted(maps.Keys(positions))
}

// joinCapped renders paths capped so a huge PR can't blow context.
func joinCapped(paths []string) string {
	if len(paths) == 0 {
		return "(none - this PR has no changed files with a diff)"
	}
	const maxShown = 30
	if len(paths) > maxShown {
		return strings.Join(paths[:maxShown], ", ") + fmt.Sprintf(", … (%d more)", len(paths)-maxShown)
	}
	return strings.Join(paths, ", ")
}

// validateLocation checks line is commentable on the given side of path.
func validateLocation(positions map[string]diffPositions, path string, line int, side string) error {
	dp, ok := positions[path]
	if !ok {
		return fmt.Errorf("github_add_review_comment: %q is not a changed file in this PR - inline comments must target a file in the diff", path)
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

// nearestCommentableLine finds the commentable line closest to target, ties toward the lower
// line for determinism. ok is false when the file has no commentable line at all.
func nearestCommentableLine(lines map[int]bool, target int) (nearest int, ok bool) {
	bestDist := -1
	for _, n := range slices.Sorted(maps.Keys(lines)) {
		if d := max(n-target, target-n); bestDist == -1 || d < bestDist {
			nearest, bestDist = n, d
		}
	}
	return nearest, bestDist != -1
}

// describeLines renders a sorted summary of commentable line numbers (capped).
func describeLines(lines map[int]bool) string {
	if len(lines) == 0 {
		return "(none - file has no commentable lines)"
	}
	var parts []string
	for _, n := range slices.Sorted(maps.Keys(lines)) {
		parts = append(parts, strconv.Itoa(n))
	}
	return joinCapped(parts)
}

type submitReviewArgs struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	PullNumber int    `json:"pull_number"`
	Body       string `json:"body,omitempty"`
	Event      string `json:"event"`
	// Gate-supplied inline findings, posted alongside the review.
	Comments []reviewComment `json:"-"`
	// ChatID is the run this review came from, for withFooter's run link.
	// Never a tool field - submitReview is gate-owned, never a model tool.
	ChatID string `json:"-"`
}

type submitReviewResult struct {
	URL      string `json:"url"`
	ReviewID int64  `json:"review_id"`
	Comments int    `json:"comments"`
}

// submitReview posts the review draft — called only post-judge-pass.
func (a *App) submitReview(ctx context.Context, args submitReviewArgs) (submitReviewResult, error) {
	if args.Owner == "" || args.Repo == "" || args.PullNumber == 0 {
		return submitReviewResult{}, fmt.Errorf("github_submit_review: owner, repo and pull_number are all required")
	}
	event := strings.ToUpper(strings.TrimSpace(args.Event))
	if !reviewEvents[event] {
		return submitReviewResult{}, fmt.Errorf("github_submit_review: event must be one of COMMENT, REQUEST_CHANGES, APPROVE; got %q", args.Event)
	}
	comments := args.Comments
	body := strings.TrimSpace(args.Body)
	if body == "" {
		// Empty body guard — never post a review with no summary.
		body = defaultReviewBody(event, len(comments))
	}
	// Marker lets a later run find this review; both marker and footer are unanchored substring
	// lookups, so their order doesn't matter.
	marker := "\n\n" + deliveryMarker("review")
	url, id, err := a.createReview(ctx, args.Owner, args.Repo, args.PullNumber, event, a.withReviewFooter(body+marker, args.ChatID), comments)
	if err != nil && len(comments) > 0 && strings.Contains(err.Error(), "status 422") {
		// One bad anchor 422s the whole review and GitHub never says which (e.g. a mid-run push moved
		// lines), so put every finding in the body: a located bullet beats losing the review.
		slog.Warn("github: delivery: review rejected on its inline anchors; posting every finding in the summary instead",
			"component", "github", "repo", args.Owner+"/"+args.Repo, "pr", args.PullNumber, "findings", len(comments), "err", err)
		stranded := make([]sdk.ReviewComment, len(comments))
		for i, c := range comments {
			stranded[i] = sdk.ReviewComment{Path: c.Path, Line: c.Line, Body: c.Body}
		}
		body += renderUnanchoredFindings(stranded)
		comments = nil
		url, id, err = a.createReview(ctx, args.Owner, args.Repo, args.PullNumber, event, a.withReviewFooter(body+marker, args.ChatID), nil)
	}
	if err != nil {
		return submitReviewResult{}, err
	}
	return submitReviewResult{URL: url, ReviewID: id, Comments: len(comments)}, nil
}

// deliveryMarker is the hidden HTML marker a later run uses to find its own prior post.
func deliveryMarker(family string) string {
	return "<!-- quack:delivery:" + family + " -->"
}

// deliveryKeyMarker embeds the idempotency key (artifact id + revision) apart from the family
// marker, so reviewVerdictMarkerRe et al. never see it. "" key embeds nothing.
func deliveryKeyMarker(key string) string {
	if key == "" {
		return ""
	}
	return "\n<!-- quack:delivery:key:" + key + " -->"
}

// deliveryKeyMarkerRe extracts a review's embedded idempotency key, if any.
var deliveryKeyMarkerRe = regexp.MustCompile(`<!-- quack:delivery:key:([^\s]+) -->`)

// RecoverDelivery implements sdk.DeliveryRecoverer: a review already carrying key means the
// crash came after the post landed (found=true, don't redeliver).
func (a *App) RecoverDelivery(ctx context.Context, key string, dc sdk.DeliveryContext) (bool, sdk.DeliveryItemOutcome, error) {
	owner, repo, ok := ownerRepoFromURL(dc.CloneURL)
	if !ok || dc.IssueNumber == 0 {
		return false, sdk.DeliveryItemOutcome{}, fmt.Errorf("github: recover: %q/#%d is not a recoverable PR reference", dc.CloneURL, dc.IssueNumber)
	}
	reviews, err := a.listReviews(ctx, owner, repo, dc.IssueNumber)
	if err != nil {
		return false, sdk.DeliveryItemOutcome{}, err
	}
	for _, r := range reviews {
		if m := deliveryKeyMarkerRe.FindStringSubmatch(r.Body); m != nil && m[1] == key {
			return true, sdk.DeliveryItemOutcome{Kind: "review", URL: r.HTMLURL}, nil
		}
	}
	return false, sdk.DeliveryItemOutcome{}, nil
}

// collapsePriorReviews minimizes prior quack-authored reviews before submitting a new one. Best-effort.
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

// collapsePriorComments minimizes quack-authored comments carrying marker family (e.g. superseded plan). Best-effort.
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

// findQuackComment returns the ID of an existing quack-authored comment with marker (revise-before-post lookup).
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

// narrationLeadRe matches process narration standing in for a real first line.
var narrationLeadRe = regexp.MustCompile(`(?i)^(I've|I have|I need to|I'll|I will|Let me|Here's|Here is)\b`)

// sanitizeCommentBody strips leading narration and an outer ```markdown fence.
func sanitizeCommentBody(body string) string {
	lines := strings.Split(body, "\n")
	lines = stripFenceWrapper(lines)
	lines = stripNarrationLead(lines)
	return strings.Join(lines, "\n")
}

// stripFenceWrapper drops a leading ```markdown/```md line and its trailing ``` pair.
func stripFenceWrapper(lines []string) []string {
	start, end := 0, len(lines)-1
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if start >= end {
		return lines
	}
	switch strings.ToLower(strings.TrimSpace(lines[start])) {
	case "```markdown", "```md":
	default:
		return lines
	}
	if strings.TrimSpace(lines[end]) != "```" {
		return lines
	}
	return lines[start+1 : end]
}

// stripNarrationLead drops a narrating first non-blank line, unless nothing would remain.
func stripNarrationLead(lines []string) []string {
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) || !narrationLeadRe.MatchString(strings.TrimSpace(lines[start])) {
		return lines
	}
	rest := lines[start+1:]
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return lines
	}
	return rest
}

// deliverStagedComment posts or edits a staged comment (marker makes revise-before-post idempotent).
func (a *App) deliverStagedComment(ctx context.Context, owner, repo string, number int, slot, bodyText, chatID string) error {
	marker := deliveryMarker("comment:" + slot)
	withMarker := a.withFooter(strings.TrimSpace(sanitizeCommentBody(bodyText))+"\n\n"+marker, chatID)
	id, found, err := a.findQuackComment(ctx, owner, repo, number, marker)
	if err != nil {
		slog.Warn("github: find prior comment failed; posting fresh", "component", "github", "repo", owner+"/"+repo, "issue", number, "slot", slot, "err", err)
	}
	if found {
		return a.editIssueComment(ctx, owner, repo, id, withMarker)
	}
	return a.postIssueComment(ctx, owner, repo, number, withMarker)
}

// defaultReviewBody synthesises a one-line summary for a review submitted with an empty body.
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
		return verdict + " (1 inline comment)."
	default:
		return fmt.Sprintf("%s (%d inline comments).", verdict, n)
	}
}

// --- Reading & reacting to existing PR discussion ---

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

// newTool registers a function tool, dropping functiontool.New's unused second return.
func newTool[A, R any](name, description string, handle functiontool.Func[A, R]) tool.Tool {
	t, _ := functiontool.New[A, R](functiontool.Config{Name: name, Description: description}, handle)
	return t
}

var replyToolDescription = "Reply in-thread to an existing inline review comment (acknowledge, agree, add context) " +
	"instead of opening a new thread. `comment_id` is the review comment you're replying to (from " +
	"github_list_pr_comments). `owner`/`repo`/`pull_number` identify the PR, `body` is the reply text. " +
	"Authenticated as the app installation."

func (a *App) replyToReviewCommentTool() tool.Tool {
	return newTool[replyArgs, replyResult]("github_reply_to_review_comment", replyToolDescription,
		func(ctx adkagent.Context, args replyArgs) (replyResult, error) { return a.reply(ctx, args) })
}

func (a *App) reply(ctx context.Context, args replyArgs) (replyResult, error) {
	if err := postRefusal(ctx, "github_reply_to_review_comment"); err != nil {
		return replyResult{}, err
	}
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

var reactToolDescription = "Add an emoji reaction to a comment - a lightweight acknowledgment. `comment_id` is the " +
	"comment; `comment_type` is `review_comment` (an inline review comment) or `issue_comment` (a " +
	"conversation comment); `content` is one of +1, -1, laugh, hooray, confused, heart, rocket, eyes. " +
	"`owner`/`repo` identify the repo. Authenticated as the app installation."

func (a *App) reactToCommentTool() tool.Tool {
	return newTool[reactArgs, reactResult]("github_react_to_comment", reactToolDescription,
		func(ctx adkagent.Context, args reactArgs) (reactResult, error) { return a.react(ctx, args) })
}

func (a *App) react(ctx context.Context, args reactArgs) (reactResult, error) {
	if err := postRefusal(ctx, "github_react_to_comment"); err != nil {
		return reactResult{}, err
	}
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

// openPullRequest opens a PR and best-effort applies labels — delivery-step call only.
func (a *App) openPullRequest(ctx context.Context, owner, repo, title, head, base, body string, labels []string, draft bool) (string, int, error) {
	if base == "" {
		base = "main"
	}
	u, number, err := a.createPullRequest(ctx, owner, repo, title, head, base, body, draft)
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

// openOrUpdatePullRequest opens or updates a PR idempotently; labels only on first open.
// titleSet/bodySet false (stage_push) leaves that field of an existing PR untouched.
func (a *App) openOrUpdatePullRequest(ctx context.Context, owner, repo, title string, titleSet bool, head, base, body string, bodySet bool, labels []string, draft bool, closesIssue int) (url string, number int, err error) {
	num, foundURL, ok, ferr := a.findOpenPR(ctx, owner, repo, head)
	if ferr != nil {
		slog.Warn("github: check for an existing open PR failed", "component", "github", "repo", owner+"/"+repo, "branch", head, "err", ferr)
	} else if ok {
		if !titleSet && !bodySet {
			slog.Info("github: pushed to the existing open pull request; nothing to update",
				"component", "github", "repo", owner+"/"+repo, "pr", num, "url", foundURL)
			return foundURL, num, nil
		}
		u, uerr := a.updatePullRequest(ctx, owner, repo, num, title, titleSet, body, bodySet)
		if uerr != nil {
			return "", 0, fmt.Errorf("update existing pull request #%d: %w", num, uerr)
		}
		slog.Info("github: updated the existing open pull request instead of opening a duplicate",
			"component", "github", "repo", owner+"/"+repo, "pr", num, "url", u)
		return u, num, nil
	}
	// No open PR (or the lookup failed) and no title to open one with: refuse rather than open a
	// titleless PR (GitHub 422s) or invent a title.
	if !titleSet {
		if ferr != nil {
			return "", 0, fmt.Errorf("github: delivery: staged a push with no title against branch %q, and checking for its open pull request failed: %w", head, ferr)
		}
		return "", 0, fmt.Errorf("github: delivery: staged a push with no title against branch %q, but no open pull request was found there - nothing to push onto", head)
	}
	body = a.withClosesTrailer(ctx, owner, repo, closesIssue, body)
	return a.openPullRequest(ctx, owner, repo, title, head, base, body, labels, draft)
}

// closesKeywordRe/closesReferences detect whether body already references issueNum with a closing keyword.
var closesKeywordRe = regexp.MustCompile(`(?i)\b(close[sd]?|fixe?[sd]?|resolve[sd]?)\b`)

func closesReferences(body string, issueNum int) bool {
	numRe := regexp.MustCompile(fmt.Sprintf(`#%d\b`, issueNum))
	for _, line := range strings.Split(body, "\n") {
		if closesKeywordRe.MatchString(line) && numRe.MatchString(line) {
			return true
		}
	}
	return false
}

// withClosesTrailer appends `Closes #N` to a new PR's body (the model drops it ~1 in 3);
// skipped when already present, for a PR, or with the partial-fix label.
func (a *App) withClosesTrailer(ctx context.Context, owner, repo string, issueNum int, body string) string {
	if issueNum == 0 || closesReferences(body, issueNum) {
		return body
	}
	_, _, _, labels, isPR, err := a.issueMeta(ctx, owner, repo, issueNum)
	if err != nil {
		slog.Warn("github: delivery: couldn't check the partial-fix label before appending Closes #N; leaving the body as-is",
			"component", "github", "repo", owner+"/"+repo, "issue", issueNum, "err", err)
		return body
	}
	if isPR || slices.Contains(labels, a.partialFixLabel) {
		return body
	}
	return strings.TrimRight(body, "\n") + fmt.Sprintf("\n\nCloses #%d\n", issueNum)
}

// Deliver posts staged items (PR, review, comments). The push is gate-owned; this only confirms
// GitHub reflects dc.PushedSHA. Outcomes are this extension's record, never the worker's self-report.
func (a *App) Deliver(ctx context.Context, dc sdk.DeliveryContext) (outcomes []sdk.DeliveryItemOutcome, err error) {
	detail := deliveryOutcome{branch: dc.Branch}
	defer func() {
		detail.err = err
		recordDelivery(dc.ChatID, detail)
	}()
	if len(dc.Items) == 0 {
		return nil, nil
	}
	if dc.PushError != "" {
		err = fmt.Errorf("github: delivery: push %q: %s", dc.Branch, dc.PushError)
		return itemOutcomesForPushFailure(dc, err), err
	}
	owner, repo, ok := ownerRepoFromURL(dc.CloneURL)
	if !ok {
		return nil, fmt.Errorf("github: delivery: %q is not a github.com clone URL - nothing to deliver against", dc.CloneURL)
	}
	// Recover PR number from chatID for ACP workers (no github_* calls).
	if dc.IssueNumber == 0 {
		dc.IssueNumber = prNumberFromChatID(dc.ChatID)
	}
	if dc.PushedSHA != "" {
		remoteSHA, verr := a.verifyPushedBranch(ctx, owner, repo, dc.Branch)
		if verr != nil {
			err = fmt.Errorf("github: delivery: push %q: verify against GitHub: %w", dc.Branch, verr)
			return itemOutcomesForPushFailure(dc, err), err
		}
		if !strings.HasPrefix(remoteSHA, dc.PushedSHA) {
			err = fmt.Errorf("github: delivery: push %q: local head %s not reflected on GitHub (remote head %s) - not delivering", dc.Branch, dc.PushedSHA, remoteSHA)
			return itemOutcomesForPushFailure(dc, err), err
		}
		detail.pushedSHA = remoteSHA
	}
	var errs []error
	outcomes = make([]sdk.DeliveryItemOutcome, len(dc.Items))
	for i, item := range dc.Items {
		res, ierr := a.deliverOne(ctx, owner, repo, dc, item)
		outcomes[i] = sdk.DeliveryItemOutcome{Kind: string(item.Kind), URL: res.url}
		if ierr != nil {
			errs = append(errs, ierr)
			outcomes[i].Error = ierr.Error()
			continue
		}
		if res.prNumber != 0 {
			detail.prNumber, detail.prURL = res.prNumber, res.prURL
			// Route review/comment to the PR we just opened, not the issue chatID.
			dc.IssueNumber = res.prNumber
		}
		if item.Kind == sdk.KindReview {
			detail.reviewDelivered = true
		}
	}
	err = errors.Join(errs...)
	return outcomes, err
}

// itemOutcomesForPushFailure marks every item as failed — push failed, nothing was attempted.
func itemOutcomesForPushFailure(dc sdk.DeliveryContext, err error) []sdk.DeliveryItemOutcome {
	out := make([]sdk.DeliveryItemOutcome, len(dc.Items))
	for i, item := range dc.Items {
		out[i] = sdk.DeliveryItemOutcome{Kind: string(item.Kind), Error: err.Error()}
	}
	return out
}

// validComments splits findings into inline and unanchored, re-anchoring an uncommentable line to the
// nearest commentable one (true location in the body). Exact duplicates are deduped.
func (a *App) validComments(ctx context.Context, owner, repo string, number int, comments []sdk.ReviewComment) (inline []reviewComment, unanchored []sdk.ReviewComment) {
	if len(comments) == 0 {
		return nil, nil
	}
	positions, err := a.commentablePositions(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("github: delivery: PR diff unavailable; keeping findings unanchored in the review body instead of dropping them",
			"component", "github", "repo", owner+"/"+repo, "pr", number, "err", err)
		return nil, comments
	}
	seen := make(map[reviewComment]bool, len(comments))
	for _, c := range comments {
		path, rerr := resolvePath(positions, c.Path)
		if rerr != nil {
			slog.Warn("github: delivery: dropping an inline finding with an unresolvable path",
				"component", "github", "path", c.Path, "err", rerr)
			continue
		}
		line, body := c.Line, c.Body
		if verr := validateLocation(positions, path, line, "RIGHT"); verr != nil {
			nearest, ok := nearestCommentableLine(positions[path].right, line)
			if !ok {
				slog.Info("github: delivery: file has no commentable line; keeping the finding unanchored in the review body",
					"component", "github", "path", path, "line", line)
				unanchored = append(unanchored, sdk.ReviewComment{Path: path, Line: line, Body: body})
				continue
			}
			slog.Info("github: delivery: re-anchoring a finding off its uncommentable line to the nearest commentable one",
				"component", "github", "path", path, "from_line", line, "to_line", nearest)
			body = fmt.Sprintf("_(this concerns line %d - anchored here because GitHub does not allow an inline comment on line %d)_\n\n%s", line, line, body)
			line = nearest
		}
		rc := reviewComment{Path: path, Line: line, Body: body}
		if seen[rc] {
			slog.Warn("github: delivery: dropping an exact-duplicate inline finding",
				"component", "github", "path", path, "line", line)
			continue
		}
		seen[rc] = true
		inline = append(inline, rc)
	}
	return inline, unanchored
}

// renderUnanchoredFindings renders findings with no commentable line as a separate review-body
// block, never prose, so a later fix run still sees them as located findings.
func renderUnanchoredFindings(findings []sdk.ReviewComment) string {
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\n\n**Unanchored findings** (GitHub would not accept an inline comment anywhere in these files):\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "- `%s` line %d: %s\n", f.Path, f.Line, f.Body)
	}
	return b.String()
}

// githubChatIDRe matches a "github-<owner>-<repo>-<number>" session id, optionally namespaced
// as the global chat id "ext:github:...".
var githubChatIDRe = regexp.MustCompile(`^(?:ext:github:)?github-.+-(\d+)$`)

func prNumberFromChatID(chatID string) int {
	m := githubChatIDRe.FindStringSubmatch(chatID)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// deliveryItemResult is what one staged item's delivery produced (PR number/url, review/comment url).
type deliveryItemResult struct {
	prNumber int
	prURL    string
	url      string
}

// gateCaveat prepends a WARNING when the trust gate failed, or a NOTE when it passed with no
// build/test check, so an unchecked pass doesn't read like a clean compile.
func gateCaveat(dc sdk.DeliveryContext, body string) string {
	if dc.GatePassed {
		if dc.ChecksSkipNote == "" {
			return body
		}
		note := "> [!NOTE]\n> " + strings.ReplaceAll(dc.ChecksSkipNote, "\n", "\n> ") + "\n\n---\n\n"
		return note + body
	}
	fb := strings.TrimSpace(dc.GateFeedback)
	if fb == "" {
		fb = "(no specific feedback was recorded - inspect the diff and tests carefully)"
	}
	banner := "> [!WARNING]\n" +
		"> **Trust gate did not pass.** Delivered for a human decision; concerns:\n" +
		">\n> " + strings.ReplaceAll(fb, "\n", "\n> ") + "\n\n---\n\n"
	return banner + body
}

// deliverOne posts one staged item past Deliver's push.
func (a *App) deliverOne(ctx context.Context, owner, repo string, dc sdk.DeliveryContext, item sdk.StagedDelivery) (deliveryItemResult, error) {
	switch item.Kind {
	case sdk.KindPR:
		return a.deliverPR(ctx, owner, repo, dc, item)
	case sdk.KindReview:
		return a.deliverReview(ctx, owner, repo, dc, item)
	case sdk.KindComment:
		return a.deliverComment(ctx, owner, repo, dc, item)
	default:
		return deliveryItemResult{}, fmt.Errorf("github: delivery: unknown staged kind %q", item.Kind)
	}
}

// deliverPR opens the staged PR from its branch. An omitted body (stage_push) skips the caveat
// so updatePullRequest reads it as "don't touch this key".
func (a *App) deliverPR(ctx context.Context, owner, repo string, dc sdk.DeliveryContext, item sdk.StagedDelivery) (deliveryItemResult, error) {
	if dc.Branch == "" {
		return deliveryItemResult{}, fmt.Errorf("github: delivery: staged pull request %q has no branch to open it from", item.Title)
	}
	body := item.Body
	if !item.BodyOmitted {
		body = gateCaveat(dc, item.Body)
	}
	// Gate-failed delivers as draft; issueNumber is the closing target for a new PR.
	u, num, err := a.openOrUpdatePullRequest(ctx, owner, repo, item.Title, !item.TitleOmitted, dc.Branch, "", body, !item.BodyOmitted, nil, !dc.GatePassed, dc.IssueNumber)
	if err != nil {
		return deliveryItemResult{}, fmt.Errorf("github: delivery: open pull request: %w", err)
	}
	slog.Info("github: delivered a pull request", "component", "github", "repo", owner+"/"+repo, "pr", num, "url", u)
	return deliveryItemResult{prNumber: num, prURL: u, url: u}, nil
}

// deliverReview submits the staged review against its PR, falling back to
// the own-PR path when the author is the bot itself.
func (a *App) deliverReview(ctx context.Context, owner, repo string, dc sdk.DeliveryContext, item sdk.StagedDelivery) (deliveryItemResult, error) {
	if dc.IssueNumber == 0 {
		return deliveryItemResult{}, fmt.Errorf("github: delivery: staged review has no pull request number to submit against")
	}
	// GitHub rejects approve/request_changes on own PR (422) — fall back to COMMENT-event review with inline comments.
	if bot, berr := a.botLogin(ctx); berr == nil {
		if author, aerr := a.prAuthor(ctx, owner, repo, dc.IssueNumber); aerr == nil && author == bot {
			return a.deliverSelfReview(ctx, owner, repo, dc, item)
		}
	}
	event := strings.ToUpper(item.Event)
	if !reviewEvents[event] {
		return deliveryItemResult{}, fmt.Errorf("github: delivery: staged review event %q is not one of approve/request_changes/comment", item.Event)
	}
	a.collapsePriorReviews(ctx, owner, repo, dc.IssueNumber) // superseded prior attempts
	// Validate inline findings before submit — one bad anchor 422s the whole review.
	inline, unanchored := a.validComments(ctx, owner, repo, dc.IssueNumber, item.Comments)
	a.observeReview(ctx, owner, repo, dc.IssueNumber, event, item.Comments)
	body := item.Body + renderUnanchoredFindings(unanchored) + deliveryKeyMarker(dc.IdempotencyKey)
	res, err := a.submitReview(ctx, submitReviewArgs{Owner: owner, Repo: repo, PullNumber: dc.IssueNumber, Body: gateCaveat(dc, body), Event: event, Comments: inline, ChatID: dc.ChatID})
	if err != nil {
		return deliveryItemResult{}, fmt.Errorf("github: delivery: submit review: %w", err)
	}
	slog.Info("github: delivered a review", "component", "github", "repo", owner+"/"+repo, "url", res.URL)
	return deliveryItemResult{url: res.URL}, nil
}

// deliverSelfReview posts the own-PR fallback: a COMMENT-event review carrying the verdict as
// text, since GitHub 422s a self-review verdict.
func (a *App) deliverSelfReview(ctx context.Context, owner, repo string, dc sdk.DeliveryContext, item sdk.StagedDelivery) (deliveryItemResult, error) {
	verdict := strings.ToLower(strings.TrimSpace(item.Event))
	if !reviewEvents[strings.ToUpper(verdict)] {
		verdict = "comment"
	}
	body := fmt.Sprintf("_Own PR: GitHub allows no self-review verdict. Verdict: %s. A maintainer decides._\n\n", verdict) + StripVerdictTail(item.Body)
	body += "\n\n" + deliveryMarker("review:"+verdict) + deliveryKeyMarker(dc.IdempotencyKey)
	// Head at delivery time, not necessarily the one reviewed (DeliveryContext has no reviewed head);
	// the synchronize re-review is the backstop.
	if m, merr := a.pullMeta(ctx, owner, repo, dc.IssueNumber); merr == nil && m.HeadSHA != "" {
		body += "\n" + deliveryMarker("head:"+m.HeadSHA)
	}
	a.collapsePriorReviews(ctx, owner, repo, dc.IssueNumber) // superseded prior attempts
	inline, unanchored := a.validComments(ctx, owner, repo, dc.IssueNumber, item.Comments)
	body += renderUnanchoredFindings(unanchored)
	a.observeReview(ctx, owner, repo, dc.IssueNumber, verdict, item.Comments)
	res, err := a.submitReview(ctx, submitReviewArgs{Owner: owner, Repo: repo, PullNumber: dc.IssueNumber, Body: gateCaveat(dc, body), Event: "COMMENT", Comments: inline, ChatID: dc.ChatID})
	if err != nil {
		return deliveryItemResult{}, fmt.Errorf("github: delivery: self-review: %w", err)
	}
	slog.Info("github: self-review delivered as a COMMENT-event review (no formal verdict - own PR)",
		"component", "github", "repo", owner+"/"+repo, "pr", dc.IssueNumber, "verdict", verdict)
	return deliveryItemResult{url: res.URL}, nil
}

// deliverComment posts the staged comment. A comment has no draft-equivalent
// lever, so the gate banner is the only unvetted signal.
func (a *App) deliverComment(ctx context.Context, owner, repo string, dc sdk.DeliveryContext, item sdk.StagedDelivery) (deliveryItemResult, error) {
	if dc.IssueNumber == 0 {
		return deliveryItemResult{}, fmt.Errorf("github: delivery: staged comment %q has no issue/PR number to post to", item.Slot)
	}
	if err := a.deliverStagedComment(ctx, owner, repo, dc.IssueNumber, item.Slot, gateCaveat(dc, item.Body), dc.ChatID); err != nil {
		return deliveryItemResult{}, fmt.Errorf("github: delivery: post comment %q: %w", item.Slot, err)
	}
	return deliveryItemResult{}, nil
}

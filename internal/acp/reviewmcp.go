package acp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/vetting"
)

// The review MCP surface gives the external code-reviewer an agentic channel
// for inline comments + verdict, riding the same loopback server as memory.

// Bare tool names, shared between the mcp.AddTool registrations below and
// mcpToolNames (acp.go, #688) - see memorymcp.go's matching const block.
const (
	toolStageReviewComment   = "stage_review_comment"
	toolListReviewComments   = "list_review_comments"
	toolUnstageReviewComment = "unstage_review_comment"
	toolStageReview          = "stage_review"
	toolStagePR              = "stage_pr"
	toolStagePush            = "stage_push"
)

// stageReviewCommentInput is stage_review_comment's input: one inline finding.
type stageReviewCommentInput struct {
	Path string `json:"path" jsonschema:"repo-relative path of the file the comment anchors to"`
	Line int    `json:"line" jsonschema:"1-based line number in that file"`
	Body string `json:"body" jsonschema:"the inline finding at that line"`
}

// listReviewCommentsInput is list_review_comments' input: plain limit/offset
// pagination - this repo's convention for bounding a response rather than
// trusting it stays small (see judge.go's changedFilesBudget, workspace's
// max_read_kb). No cursor: a review node stages at most a few dozen findings.
type listReviewCommentsInput struct {
	Limit  int `json:"limit,omitempty" jsonschema:"max comments to return (default 50)"`
	Offset int `json:"offset,omitempty" jsonschema:"how many staged comments to skip, in stage order (default 0)"`
}

// unstageReviewCommentInput is unstage_review_comment's input: the id
// stage_review_comment (or list_review_comments) returned for the finding.
type unstageReviewCommentInput struct {
	ID string `json:"id" jsonschema:"the id of the staged comment to remove, from stage_review_comment or list_review_comments"`
}

// stageReviewInput is stage_review's input: the overall verdict + summary.
type stageReviewInput struct {
	Event string `json:"event" jsonschema:"overall verdict: approve, request_changes, or comment"`
	Body  string `json:"body" jsonschema:"the review summary posted alongside the verdict"`
}

// defaultListLimit and listExcerptLen bound list_review_comments' response:
// a page of results, and a short excerpt per body rather than the full text -
// just enough for the reviewer to recognize a finding it already staged and
// grab the id to retract it with, not to reproduce the finding verbatim
// (unstage_review_comment takes the id, not the body, so it never needs to).
const (
	defaultListLimit = 50
	listExcerptLen   = 120
)

// excerpt truncates s to n runes, marking truncation with a trailing "…".
func excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// registerReviewTools adds review staging tools to a per-node server.
func registerReviewTools(srv *mcp.Server, review *vetting.ReviewStage) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolStageReviewComment,
		Description: "Stage one inline, line-anchored review comment on the pull request under review. Call once per finding; the gate posts them after your answer passes. Returns the id of the staged comment, for later retraction via unstage_review_comment.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args stageReviewCommentInput) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Path) == "" || args.Line <= 0 || strings.TrimSpace(args.Body) == "" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "stage_review_comment needs a path, a positive line, and a non-empty body"}}}, nil, nil
		}
		id := review.AddComment(strings.TrimSpace(args.Path), args.Line, strings.TrimSpace(args.Body))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("staged as id %s", id)}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolListReviewComments,
		Description: "List comments staged so far, in stage order, paginated (default 50 per page). Each entry has an id, path, line, and a short excerpt of the body. Call this before staging a new finding to check you haven't already recorded it; retract a duplicate with unstage_review_comment(id).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args listReviewCommentsInput) (*mcp.CallToolResult, any, error) {
		limit := args.Limit
		if limit <= 0 {
			limit = defaultListLimit
		}
		offset := args.Offset
		if offset < 0 {
			offset = 0
		}
		all := review.ListComments()
		total := len(all)
		var page []vetting.StagedReviewComment
		if offset < total {
			end := offset + limit
			if end > total {
				end = total
			}
			page = all[offset:end]
		}
		if total == 0 {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no comments staged yet"}}}, nil, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "showing %d of %d staged comments (offset %d):\n", len(page), total, offset)
		for _, c := range page {
			fmt.Fprintf(&b, "id=%s path=%s line=%d excerpt=%q\n", c.ID, c.Path, c.Line, excerpt(c.Body, listExcerptLen))
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolUnstageReviewComment,
		Description: "Retract a previously staged inline comment by id (from stage_review_comment or list_review_comments) - e.g. after re-reading the file and deciding the finding doesn't hold, or because it duplicates one already staged. An unknown id is an error, not a silent no-op.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args unstageReviewCommentInput) (*mcp.CallToolResult, any, error) {
		id := strings.TrimSpace(args.ID)
		if id == "" || !review.RemoveComment(id) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("no staged comment with id %q", id)}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("unstaged %s", id)}}}, nil, nil
	})
	// A slice feeding a synthesizer never owns the delivered verdict (#1148):
	// the tool is withheld rather than registered-and-refused, so the
	// reviewer prompt's "the tool list is a fact" holds.
	if review.IsNonDeliveringSlice() {
		return
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolStageReview,
		Description: "Stage the overall review verdict (approve | request_changes | comment) and summary. Call once, after your inline comments; the gate submits the review after your answer passes.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args stageReviewInput) (*mcp.CallToolResult, any, error) {
		event := strings.ToLower(strings.TrimSpace(args.Event))
		if event != "approve" && event != "request_changes" && event != "comment" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "stage_review event must be approve, request_changes, or comment"}}}, nil, nil
		}
		if err := review.SetVerdict(event, args.Body); err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged"}}}, nil, nil
	})
}

// stagePRInput is stage_pr's input: the title + body the implementer authored
// with the pr-authoring skill (that skill owns the template - the repo's or its
// default - so nothing here is deterministic beyond requiring both fields).
type stagePRInput struct {
	Title string `json:"title" jsonschema:"the PR title: type(scope): subject, imperative, <=50 chars, references the issue"`
	Body  string `json:"body" jsonschema:"the PR description authored per the pr-authoring skill (what/why/how/verify, repo template or the skill's default)"`
}

// registerPRTool adds stage_pr to a per-node server, landing the call in PRStage.
func registerPRTool(srv *mcp.Server, pr *vetting.PRStage) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolStagePR,
		Description: "Stage the pull request title and description for this change, authored per the pr-authoring skill. Call once, after committing; the gate opens the PR with exactly this after your work passes review. You never push or open the PR yourself.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args stagePRInput) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Body) == "" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "stage_pr needs a non-empty title and body"}}}, nil, nil
		}
		pr.Set(strings.TrimSpace(args.Title), strings.TrimSpace(args.Body))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged"}}}, nil, nil
	})
}

// stagePushInput is stage_push's input: title/body are OPTIONAL - this run is
// pushing onto a PR that already exists, so it may have nothing to say about
// either (issue #724: stage_pr's required fields forced the agent to invent
// them, and the invented text then overwrote someone else's PR).
type stagePushInput struct {
	Title string `json:"title,omitempty" jsonschema:"optional: only pass this if you are deliberately changing the PR's title"`
	Body  string `json:"body,omitempty" jsonschema:"optional: only pass this if you are deliberately changing the PR's description"`
}

// registerPushTool adds stage_push to a per-node server, landing the call in
// PRStage. Registered INSTEAD of stage_pr (internal/acp/acp.go's mcpToolNames)
// when the run targets a PR that already exists.
func registerPushTool(srv *mcp.Server, pr *vetting.PRStage) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: toolStagePush,
		Description: "Stage this push against the pull request that's already open on this branch. Call once, after committing; the gate pushes it after your work passes review. " +
			"title and body are OPTIONAL - omit either (or both) to leave it exactly as it is; pass one only if you are deliberately changing it. You never push yourself.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args stagePushInput) (*mcp.CallToolResult, any, error) {
		title := strings.TrimSpace(args.Title)
		body := strings.TrimSpace(args.Body)
		pr.SetPush(title, title != "", body, body != "")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged"}}}, nil, nil
	})
}

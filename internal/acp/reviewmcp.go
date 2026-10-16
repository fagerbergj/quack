package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/vetting"
)

// The review MCP surface gives the external code-reviewer an agentic channel
// for inline comments + verdict, riding the same loopback server as memory.

// Bare tool names shared by the mcp.AddTool registrations and mcpToolNames (see memorymcp.go's block).
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
	Path  string `json:"path" jsonschema:"repo-relative path of the file the comment anchors to"`
	Line  int    `json:"line" jsonschema:"1-based line number in that file"`
	Label string `json:"label,omitempty" jsonschema:"the finding's Conventional Comments label: blocking, suggestion, nit, or question"`
	Body  string `json:"body" jsonschema:"the inline finding at that line, without its label"`

	ProposedChange *vetting.ProposedChange `json:"proposed_change,omitempty" jsonschema:"the replacement code for a finding that proposes a specific change; omit for a question or observation"`
}

// listReviewCommentsInput: plain limit/offset pagination to bound the response.
// No cursor: a review node stages at most a few dozen findings.
type listReviewCommentsInput struct {
	Limit  int `json:"limit,omitempty" jsonschema:"max comments to return (default 50)"`
	Offset int `json:"offset,omitempty" jsonschema:"how many staged comments to skip, in stage order (default 0)"`
}

// unstageReviewCommentInput is unstage_review_comment's input: the id
// stage_review_comment (or list_review_comments) returned for the finding.
type unstageReviewCommentInput struct {
	ID string `json:"id" jsonschema:"the id of the staged comment to remove, from stage_review_comment or list_review_comments"`
}

// stageReviewInput: the overall verdict plus the review format's model-written fields
// (the rest is generated; see reviewrecord.go's CodeReviewRecord).
type stageReviewInput struct {
	Event    string   `json:"event" jsonschema:"overall verdict: approve, request_changes, or comment"`
	Takeaway string   `json:"takeaway" jsonschema:"one sentence, max 240 characters - the fifteen-second takeaway, never a restatement of findings already staged inline"`
	Verified []string `json:"verified,omitempty" jsonschema:"what you actually checked, max 8 items, each max 160 characters - may start an item with 'not verified:'"`
	Notes    []string `json:"notes,omitempty" jsonschema:"free prose with no line to anchor to (architecture, praise, follow-ups, unresolved questions), max 8 items, each max 200 characters"`
	// Dismissed/Clean feed only the durable code_review record, so a re-review doesn't re-litigate them.
	Dismissed []vetting.DismissedEntry `json:"dismissed,omitempty" jsonschema:"candidates you considered and ruled out: path, line, and a note saying why"`
	Clean     []string                 `json:"clean,omitempty" jsonschema:"files you reviewed and found nothing to report in"`
}

// stageReviewSchema is stageReviewInput's inferred schema plus the caps CheckCodeReviewCaps enforces.
var stageReviewSchema = func() *jsonschema.Schema {
	s, err := jsonschema.For[stageReviewInput](nil)
	if err != nil {
		panic("acp: stage_review schema: " + err.Error())
	}
	vetting.DeclareCodeReviewCaps(s)
	return s
}()

// stageReviewCommentSchema is stageReviewCommentInput's inferred schema plus ProposedChange.Check's rules.
var stageReviewCommentSchema = func() *jsonschema.Schema {
	s, err := jsonschema.For[stageReviewCommentInput](nil)
	if err != nil {
		panic("acp: stage_review_comment schema: " + err.Error())
	}
	vetting.DeclareProposedChangeCaps(s.Properties["proposed_change"])
	for _, l := range vetting.CommentLabels {
		s.Properties["label"].Enum = append(s.Properties["label"].Enum, l)
	}
	return s
}()

// defaultListLimit and listExcerptLen bound list_review_comments: an excerpt is enough to recognize a staged
// finding and grab its id (unstage_review_comment takes the id, not the body).
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
	addStageReviewComment(srv, review)
	addListReviewComments(srv, review)
	addUnstageReviewComment(srv, review)
	// A slice feeding a synthesizer never owns the delivered verdict: withhold the tool rather than
	// register-and-refuse it, since "the tool list is a fact".
	if review.IsNonDeliveringSlice() {
		return
	}
	addStageReview(srv, review)
}

// addStageReviewComment: the stage_review_comment tool (path/line/body validation,
// label check, duplicate rejection).
func addStageReviewComment(srv *mcp.Server, review *vetting.ReviewStage) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolStageReviewComment,
		Description: "Stage one inline, line-anchored review comment on the pull request under review. Call once per finding; the gate posts them after your answer passes. Returns the id of the staged comment, for later retraction via unstage_review_comment. Duplicates (same path, line, and body) are rejected with the existing id, not double-staged.",
		InputSchema: stageReviewCommentSchema,
	}, stageReviewCommentHandler(review))
}

func stageReviewCommentHandler(review *vetting.ReviewStage) mcp.ToolHandlerFor[stageReviewCommentInput, any] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args stageReviewCommentInput) (*mcp.CallToolResult, any, error) {
		body := vetting.WithCommentLabel(strings.TrimSpace(args.Label), strings.TrimSpace(args.Body))
		if err := checkReviewComment(strings.TrimSpace(args.Path), args.Line, body, args.ProposedChange); err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
		}
		id, dup := review.AddComment(strings.TrimSpace(args.Path), args.Line, body+args.ProposedChange.Fence())
		if dup {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("duplicate of %s; not staged", id)}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("staged as id %s", id)}}}, nil, nil
	}
}

// checkReviewComment reports every problem with one stage_review_comment call, so one retry fixes them all.
func checkReviewComment(path string, line int, body string, change *vetting.ProposedChange) error {
	errs := []error{change.Check()}
	if path == "" {
		errs = append(errs, errors.New("stage_review_comment: path is required"))
	}
	if line <= 0 {
		errs = append(errs, fmt.Errorf("stage_review_comment: line must be a positive 1-based line number, got %d", line))
	}
	// The one choke every reviewer node's findings share - an unlabeled
	// finding otherwise renders uncounted and never makes Highlights.
	switch {
	case body == "":
		errs = append(errs, errors.New("stage_review_comment: body is required"))
	case !vetting.HasCommentLabel(body):
		errs = append(errs, errors.New("stage_review_comment: label is required - pass label: blocking, suggestion, nit, or question"))
	}
	return errors.Join(errs...)
}

// addListReviewComments: the list_review_comments tool (paginated listing).
func addListReviewComments(srv *mcp.Server, review *vetting.ReviewStage) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolListReviewComments,
		Description: "List comments staged so far, in stage order, paginated (default 50 per page). Each entry has an id, path, line, and a short excerpt of the body.",
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
}

// addUnstageReviewComment: the unstage_review_comment tool (retract by id).
func addUnstageReviewComment(srv *mcp.Server, review *vetting.ReviewStage) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolUnstageReviewComment,
		Description: "Retract a previously staged inline comment by id (from stage_review_comment or list_review_comments) - e.g. after re-reading the file and deciding the finding doesn't hold, or because it duplicates one already staged. An unknown id is an error, not a silent no-op.",
	}, unstageReviewCommentHandler(review))
}

func unstageReviewCommentHandler(review *vetting.ReviewStage) mcp.ToolHandlerFor[unstageReviewCommentInput, any] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args unstageReviewCommentInput) (*mcp.CallToolResult, any, error) {
		id := strings.TrimSpace(args.ID)
		if id == "" || !review.RemoveComment(id) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("no staged comment with id %q", id)}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("unstaged %s", id)}}}, nil, nil
	}
}

// addStageReview: the stage_review tool (verdict/takeaway/verified/notes
// validation, then the gate's SetVerdict).
func addStageReview(srv *mcp.Server, review *vetting.ReviewStage) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: toolStageReview,
		Description: "Stage the overall review verdict (approve | request_changes | comment), takeaway, verified checks, notes, and the " +
			"dismissed candidates and clean files the code_review record keeps for re-reviews. Call it after your inline comments; calling it " +
			"again replaces the staged review. The gate renders the fixed review format (verdict/scope/highlights are generated, not written), records it as " +
			"this PR's code_review, and submits it after your answer passes. " + vetting.CodeReviewCapsText + ".",
		InputSchema: stageReviewSchema,
	}, stageReviewHandler(review))
}

func stageReviewHandler(review *vetting.ReviewStage) mcp.ToolHandlerFor[stageReviewInput, any] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args stageReviewInput) (*mcp.CallToolResult, any, error) {
		event := strings.ToLower(strings.TrimSpace(args.Event))
		takeaway := strings.TrimSpace(args.Takeaway)
		if err := checkStageReview(event, takeaway, args.Verified, args.Notes); err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
		}
		if err := review.SetVerdict(event, takeaway, args.Verified, args.Notes, args.Dismissed, args.Clean); err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged"}}}, nil, nil
	}
}

// ReplayReviewCall applies one recorded review-tool call to review through the live handler, which refuses
// it as it would live; other tool names are ignored. false: the args did not decode and the call was dropped.
func ReplayReviewCall(review *vetting.ReviewStage, name, args string) bool {
	switch name {
	case toolStageReviewComment:
		return replayCall(args, stageReviewCommentHandler(review))
	case toolUnstageReviewComment:
		return replayCall(args, unstageReviewCommentHandler(review))
	case toolStageReview:
		return replayCall(args, stageReviewHandler(review))
	}
	return true
}

func replayCall[In any](args string, h mcp.ToolHandlerFor[In, any]) bool {
	var in In
	if json.Unmarshal([]byte(args), &in) != nil {
		return false
	}
	_, _, _ = h(context.Background(), nil, in)
	return true
}

// checkStageReview reports every problem with one stage_review call, so one retry fixes them all.
func checkStageReview(event, takeaway string, verified, notes []string) error {
	var errs []error
	if event != "approve" && event != "request_changes" && event != "comment" {
		errs = append(errs, errors.New("stage_review event must be approve, request_changes, or comment"))
	}
	if takeaway == "" {
		errs = append(errs, errors.New("stage_review takeaway is required: one sentence, max 240 characters"))
	}
	return errors.Join(append(errs, vetting.CheckCodeReviewCaps(takeaway, verified, notes))...)
}

// stagePRInput: the title + body the implementer authored with the pr-authoring skill, which owns the template.
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

// stagePushInput: title/body are optional because the PR already exists; required fields made agents
// invent text that overwrote someone else's PR.
type stagePushInput struct {
	Title string `json:"title,omitempty" jsonschema:"optional: only pass this if you are deliberately changing the PR's title"`
	Body  string `json:"body,omitempty" jsonschema:"optional: only pass this if you are deliberately changing the PR's description"`
}

// registerPushTool adds stage_push, landing the call in PRStage; registered instead of stage_pr
// when the run targets an existing PR.
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

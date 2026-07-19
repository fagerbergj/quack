package acp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/vetting"
)

// The review MCP surface (#451) gives the EXTERNAL (ACP) code-reviewer a real
// agentic channel for its deliverable — line-anchored inline comments and a
// verdict — instead of the gate reverse-engineering them out of a VERDICT:/
// FINDINGS: tail in the answer (vetting.augmentFromAnswer, still the fallback).
// It rides the SAME per-node loopback server and secret as the memory surface
// (memorymcp.go); registerReviewTools is called only when the node's session
// carries a ReviewStage — minted exclusively for a review-delivery node, so the
// implementer/explorer and every native agent never see these tools.

// stageReviewCommentInput is stage_review_comment's input: one inline finding.
type stageReviewCommentInput struct {
	Path string `json:"path" jsonschema:"repo-relative path of the file the comment anchors to"`
	Line int    `json:"line" jsonschema:"1-based line number in that file"`
	Body string `json:"body" jsonschema:"the inline finding at that line"`
}

// stageReviewInput is stage_review's input: the overall verdict + summary.
type stageReviewInput struct {
	Event string `json:"event" jsonschema:"overall verdict: approve, request_changes, or comment"`
	Body  string `json:"body" jsonschema:"the review summary posted alongside the verdict"`
}

// registerReviewTools adds stage_review_comment + stage_review to a per-node
// server, landing calls in the node's ReviewStage. The gate snapshots that
// buffer into the staged review after the answer passes (vetting).
func registerReviewTools(srv *mcp.Server, review *vetting.ReviewStage) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "stage_review_comment",
		Description: "Stage one inline, line-anchored review comment on the pull request under review. Call once per finding; the gate posts them after your answer passes.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args stageReviewCommentInput) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Path) == "" || args.Line <= 0 || strings.TrimSpace(args.Body) == "" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "stage_review_comment needs a path, a positive line, and a non-empty body"}}}, nil, nil
		}
		review.AddComment(strings.TrimSpace(args.Path), args.Line, strings.TrimSpace(args.Body))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged"}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "stage_review",
		Description: "Stage the overall review verdict (approve | request_changes | comment) and summary. Call once, after your inline comments; the gate submits the review after your answer passes.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args stageReviewInput) (*mcp.CallToolResult, any, error) {
		event := strings.ToLower(strings.TrimSpace(args.Event))
		if event != "approve" && event != "request_changes" && event != "comment" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "stage_review event must be approve, request_changes, or comment"}}}, nil, nil
		}
		review.SetVerdict(event, args.Body)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged"}}}, nil, nil
	})
}

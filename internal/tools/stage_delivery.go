package tools

import (
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// The staged-delivery tools: workers commit locally and STAGE delivery intent;
// they never push, open a
// pull request, or submit a review themselves. Each call, like stage_memory,
// is a SINK — it records nothing itself, only lands in the session — and the
// trust gate (internal/vetting) harvests the FINAL staged set from there,
// posting it exactly once, only on a judge pass (see commitDeliveryOnPass).
// An item staged then unstage'd, or replaced by a later stage_* call, never
// reaches GitHub.

type stagePRArgs struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func newStagePR(_ Deps) (tool.Tool, error) {
	return functiontool.New[stagePRArgs, string](
		functiontool.Config{
			Name: "stage_pr",
			Description: "Stage the pull request you want opened for your committed work — `title` and `body` (the " +
				"PR description). The branch is the one you committed on. Re-calling this REPLACES any prior stage_pr " +
				"call (revise-before-post). Nothing is opened on GitHub until your answer passes review; you never push " +
				"or open pull requests yourself.",
		},
		func(_ agent.Context, a stagePRArgs) (string, error) {
			if strings.TrimSpace(a.Title) == "" {
				return "", fmt.Errorf("stage_pr: title is empty")
			}
			return "Staged for delivery (a pull request is opened only if your answer passes review).", nil
		},
	)
}

type stageReviewArgs struct {
	Event string `json:"event"` // approve | request_changes | comment
	Body  string `json:"body"`
}

func newStageReview(_ Deps) (tool.Tool, error) {
	return functiontool.New[stageReviewArgs, string](
		functiontool.Config{
			Name: "stage_review",
			Description: "Stage the pull request review you want submitted — `event` (approve | request_changes | " +
				"comment) and `body` (your summary). Record inline findings with github_add_review_comment first; this " +
				"stages the SUBMIT. Re-calling this REPLACES any prior stage_review call. Nothing is posted until your " +
				"answer passes review; you never submit reviews yourself.",
		},
		func(_ agent.Context, a stageReviewArgs) (string, error) {
			event := strings.ToLower(strings.TrimSpace(a.Event))
			if !stageReviewEvents[event] {
				return "", fmt.Errorf("stage_review: event must be one of approve, request_changes, comment; got %q", a.Event)
			}
			return "Staged for delivery (the review is submitted only if your answer passes review).", nil
		},
	)
}

var stageReviewEvents = map[string]bool{"approve": true, "request_changes": true, "comment": true}

type stageCommentArgs struct {
	Slot string `json:"slot"`
	Body string `json:"body"`
}

func newStageComment(_ Deps) (tool.Tool, error) {
	return functiontool.New[stageCommentArgs, string](
		functiontool.Config{
			Name: "stage_comment",
			Description: "Stage a comment to post — `slot` names it (e.g. \"progress\", \"summary\"; your choice), " +
				"`body` is the text. Re-calling with the same `slot` REPLACES that comment. Nothing is posted until " +
				"your answer passes review.",
		},
		func(_ agent.Context, a stageCommentArgs) (string, error) {
			if strings.TrimSpace(a.Slot) == "" {
				return "", fmt.Errorf("stage_comment: slot is empty")
			}
			if strings.TrimSpace(a.Body) == "" {
				return "", fmt.Errorf("stage_comment: body is empty")
			}
			return "Staged for delivery (posted only if your answer passes review).", nil
		},
	)
}

type unstageArgs struct {
	Target string `json:"target"` // pr | review | comment:<slot>
}

func newUnstage(_ Deps) (tool.Tool, error) {
	return functiontool.New[unstageArgs, string](
		functiontool.Config{
			Name: "unstage",
			Description: "Drop a previously staged delivery item you've decided against — `target` is \"pr\", " +
				"\"review\", or \"comment:<slot>\". A dropped item is never posted, even if your answer later passes review.",
		},
		func(_ agent.Context, a unstageArgs) (string, error) {
			if strings.TrimSpace(a.Target) == "" {
				return "", fmt.Errorf("unstage: target is empty")
			}
			return "Unstaged.", nil
		},
	)
}

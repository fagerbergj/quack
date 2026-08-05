package tools

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// ChoiceToolName: user-choice clarification tool name, referenced by the orchestrator.
const ChoiceToolName = "get_user_choice"

// ChoiceAnswerKey: key distinguishing a real answer from a pending placeholder.
const ChoiceAnswerKey = "choice"

type getUserChoiceArgs struct {
	Question string `json:"question"`
	Options []string `json:"options"`
}

type getUserChoiceResult struct {
	Status string `json:"status"`
}

// NewGetUserChoiceTool: long-running clarification tool - presents options and pauses until answered.
func NewGetUserChoiceTool() (tool.Tool, error) {
	return functiontool.New[getUserChoiceArgs, getUserChoiceResult](
		functiontool.Config{
			Name: ChoiceToolName,
			Description: "Asks the user a clarifying question with a set of options to choose from. " +
				"Use when a request is genuinely ambiguous and the resolution is one of a few discrete choices " +
				"(e.g. which 'Springfield', which interpretation) - the answer must materially change the work. " +
				"Pass the `question` (so the options have context) and the candidate `options`; " +
				"the user's selection is returned to you and you continue. " +
				"Do NOT use when a sensible default exists or the task is already clear.",
			IsLongRunning: true,
		},
		func(tc agent.Context, a getUserChoiceArgs) (getUserChoiceResult, error) {
			tc.Actions().SkipSummarization = true
			return getUserChoiceResult{Status: "pending"}, nil
		},
	)
}

package tools

import (
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ChoiceToolName is the name of the user-choice clarification tool. The
// orchestrator references it to detect and resume a pending clarification.
const ChoiceToolName = "get_user_choice"

// ChoiceAnswerKey is the response key under which a resumed answer is delivered.
// The auto-emitted pending placeholder uses a different shape ({"status":
// "pending"}), so the presence of this key distinguishes a real answer from the
// placeholder when the orchestrator scans the session.
const ChoiceAnswerKey = "choice"

type getUserChoiceArgs struct {
	// Options are the choices to present to the user.
	Options []string `json:"options"`
}

type getUserChoiceResult struct {
	// Status is "pending": the real choice is supplied later, when the human
	// answers, as a FunctionResponse carrying ChoiceAnswerKey (see the
	// orchestrator's resume path). A long-running tool returns immediately with a
	// placeholder; its call is left open until that response arrives.
	Status string `json:"status"`
}

// NewGetUserChoiceTool returns a long-running clarification tool: it presents a
// set of options to the user and pauses the turn until the user chooses one.
//
// This is a faithful port of Python ADK's get_user_choice_tool
// (google/adk/tools/get_user_choice_tool.py):
//
//	def get_user_choice(options: list[str], tool_context) -> Optional[str]:
//	    """Provides the options to the user and asks them to choose one."""
//	    tool_context.actions.skip_summarization = True
//	    return None
//	get_user_choice_tool = LongRunningFunctionTool(func=get_user_choice)
//
// Go ADK has no native equivalent (it ships RequireConfirmation — a typed
// approve/deny gate — but no free-form/choice ask tool).
// TODO: replace this port with the Go-native get_user_choice once ADK
// implements it, and drop the orchestrator's hand-rolled resume plumbing.
//
// The handler returns a "pending" placeholder rather than None (Go has no
// nil for a value return); SkipSummarization ends the turn so the model does
// not narrate over the question. The actual choice arrives on the next turn as
// a FunctionResponse with the same call ID — the framework does NOT re-invoke
// this handler on resume.
func NewGetUserChoiceTool() (tool.Tool, error) {
	return functiontool.New[getUserChoiceArgs, getUserChoiceResult](
		functiontool.Config{
			Name: ChoiceToolName,
			Description: "Provides a set of options to the user and asks them to choose one. " +
				"Use when a request is genuinely ambiguous and the resolution is one of a few discrete choices " +
				"(e.g. which 'Springfield', which interpretation) — the answer must materially change the work. " +
				"Pass the candidate options; the user's selection is returned to you and you continue. " +
				"Do NOT use when a sensible default exists or the task is already clear.",
			IsLongRunning: true,
		},
		func(tc agent.ToolContext, a getUserChoiceArgs) (getUserChoiceResult, error) {
			tc.Actions().SkipSummarization = true
			return getUserChoiceResult{Status: "pending"}, nil
		},
	)
}

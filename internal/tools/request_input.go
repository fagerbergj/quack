package tools

import (
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// RequestInputToolName is the name of the mid-DAG human-input tool. The gate
// detects its pending (long-running) call to pause a node, and the resume path
// answers it with a FunctionResponse carrying RequestInputAnswerKey.
const RequestInputToolName = "request_input"

// RequestInputAnswerKey is the response key under which a resumed answer is
// delivered (mirrors ChoiceAnswerKey for get_user_choice). The auto-emitted
// pending placeholder uses {"status":"pending"}; the presence of this key marks
// a real answer.
const RequestInputAnswerKey = "answer"

type requestInputArgs struct {
	// Question is the open free-form question shown to the user.
	Question string `json:"question"`
}

type requestInputResult struct {
	// Status is "pending": the real answer is supplied later, when the human
	// answers, as a FunctionResponse carrying RequestInputAnswerKey (see the
	// executor's resume path). A long-running tool returns immediately with this
	// placeholder; its call is left open until that response arrives.
	Status string `json:"status"`
}

// newRequestInput returns a long-running human-input tool a node worker can call
// to pause itself for a human answer mid-DAG. It poses an open question and
// returns immediately with a "pending" placeholder; ADK marks the call long
// running (event.LongRunningToolIDs), which the trust gate detects to suspend the
// node and the A2A v2 bridge surfaces as TaskStateInputRequired.
//
// Unlike get_user_choice (an authored orchestrator-only tool), request_input is
// registry-backed so a worker-agent bundle selects it by name — it runs inside
// node workers, not the orchestrator.
//
// ponytail: free-form ask only. ADK also ships RequireConfirmation (a typed
// approve/deny gate for side-effecting tools); that is deferred to M10, when the
// GitHub write tools that need it land.
func newRequestInput(_ Deps) (tool.Tool, error) {
	return functiontool.New[requestInputArgs, requestInputResult](
		functiontool.Config{
			Name: RequestInputToolName,
			Description: "Ask the user an open-ended question when the task genuinely cannot proceed " +
				"without information only the user has (an ambiguous instruction, a missing constraint, " +
				"a judgement call the user must make). Pass `question`; the user's answer is returned to " +
				"you and you continue. Do NOT use when a sensible default exists or the task is already clear.",
			IsLongRunning: true,
		},
		func(tc agent.ToolContext, a requestInputArgs) (requestInputResult, error) {
			tc.Actions().SkipSummarization = true
			return requestInputResult{Status: "pending"}, nil
		},
	)
}

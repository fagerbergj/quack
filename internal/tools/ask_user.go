package tools

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/vetting"
)

type askUserArgs struct {
	// Question is the specific question to put to the user.
	Question string `json:"question"`
}

type askUserResult struct {
	// Status tells the model the question was forwarded; the answer arrives in a
	// follow-up run, folded into the prompt by the trust gate.
	Status string `json:"status"`
}

// NewAskUserTool returns the mid-node HITL tool (vetting.AskToolName): a worker
// agent calls it when its task is blocked on information only the user has. The
// tool takes no action itself — it records the question (in its call args) and
// ends the worker's turn (SkipSummarization); the trust GATE detects the call,
// pauses the NODE via workflow.ResumeOrRequestInput under a round-stable
// interrupt ID, and the user's next message resumes the node with the answer
// folded into the worker's prompt.
func NewAskUserTool() (tool.Tool, error) {
	return functiontool.New[askUserArgs, askUserResult](
		functiontool.Config{
			Name: vetting.AskToolName,
			Description: "Ask the USER a clarifying question when your task is blocked on information " +
				"only they have (a credential-free choice, an ambiguous requirement, a preference that " +
				"materially changes the work). The run pauses until they answer; their answer is delivered " +
				"back to you. Ask at most one precise question, and NEVER use this when a sensible default " +
				"or your own research can resolve the ambiguity.",
		},
		func(tc agent.Context, _ askUserArgs) (askUserResult, error) {
			// End the worker's turn so the gate sees the ask (an empty draft with the
			// question in the session) and pauses the node.
			tc.Actions().SkipSummarization = true
			return askUserResult{Status: "forwarded to the user; their answer will arrive in a follow-up"}, nil
		},
	)
}

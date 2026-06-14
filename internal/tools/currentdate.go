package tools

import (
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// currentDateArgs is empty — the tool takes no input.
type currentDateArgs struct{}

// newCurrentDate builds the current_date tool: it returns today's real date so the
// agent anchors time-sensitive research in the present instead of its training
// cutoff. The system prompt also states the date, but models reliably trust a tool
// RESULT they actively fetched over a static prompt line — so for "recent/latest/
// this year" queries the tool meaningfully overcomes the training-data prior.
func newCurrentDate(_ Deps) (tool.Tool, error) {
	return functiontool.New[currentDateArgs, string](
		functiontool.Config{
			Name:        "current_date",
			Description: "Return today's real current date. Call this FIRST whenever the request is time-sensitive (mentions recent, latest, new, current, this year, etc.) so you search for the actual present and don't default to your training cutoff.",
		},
		func(_ agent.ToolContext, _ currentDateArgs) (string, error) {
			now := time.Now()
			return "Today's date is " + now.Format("Monday, January 2, 2006") + ".", nil
		},
	)
}

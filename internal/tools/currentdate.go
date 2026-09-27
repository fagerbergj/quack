package tools

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/promptbuilder"
)

type currentDateArgs struct{}

// newCurrentDate builds the current_date tool: the real date and time in the user's zone, so the agent
// anchors time-sensitive research in the present instead of its training cutoff.
// Models trust a tool RESULT they actively fetched over the static prompt line - for "recent/latest/this year" queries the tool overcomes the training-data prior.
func newCurrentDate(_ Deps) (tool.Tool, error) {
	return functiontool.New[currentDateArgs, string](
		functiontool.Config{
			Name:        "current_date",
			Description: "Return the real current date and time in the user's time zone, with the zone abbreviation, its UTC offset, the IANA zone name (e.g. America/Chicago) when one is configured, and the same instant in UTC. Call this FIRST whenever the request is time-sensitive (mentions recent, latest, new, current, this year, etc.) so you search for the actual present and don't default to your training cutoff, and before comparing against a scheduled time (kickoff, deadline, lock) - convert that time to the user's zone with the offsets, never guess the day from UTC.",
		},
		func(_ agent.Context, _ currentDateArgs) (string, error) {
			return "Current local time: " + promptbuilder.Now() + ".", nil
		},
	)
}

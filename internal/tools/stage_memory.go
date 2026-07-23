package tools

import (
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// stageMemoryArgs is one durable thing the agent wants to remember, plus the bucket
// it is ABOUT (memory is shared and subject-bucketed - see internal/memory/scope.go).
type stageMemoryArgs struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
	Bucket  string `json:"bucket"`
}

// newStageMemory builds the stage_memory tool. It is a SINK: it records nothing
// itself - the call (with its args) lands in the worker's session, and the trust
// gate harvests staged candidates from there, committing them only if the answer
// passes vetting. So nothing is ever remembered from a failed answer.
func newStageMemory(_ Deps) (tool.Tool, error) {
	return functiontool.New[stageMemoryArgs, string](
		functiontool.Config{
			Name: "stage_memory",
			Description: "Stage a durable fact to remember for future, unrelated tasks. Memory is SHARED with the " +
				"other agents, filed by what the fact is ABOUT - say which with `bucket`:\n" +
				"- `repo`: something true of the repository you are working in - a convention, its build/test/lint " +
				"commands, where a feature is registered, the reference feature to mirror, a failure that was " +
				"already broken before you touched it. Every agent that later works in THIS repo recalls it.\n" +
				"- `role`: how to do your job well, independent of any one repo (e.g. 'install dependencies before " +
				"running checks'; 'a source's own docs beat a blog post about it'). Every agent in your role recalls it.\n" +
				"- `user`: a durable fact about the user (a preference, a hard limit).\n" +
				"`content` is one atomic sentence; `kind` is a short free-form tag (e.g. convention|command|layout|" +
				"source|deadend). Do NOT stage volatile facts or request-specific answers. Staged items are reviewed " +
				"and kept only if your answer passes vetting - nothing is remembered until then.",
		},
		func(_ agent.Context, a stageMemoryArgs) (string, error) {
			if strings.TrimSpace(a.Content) == "" {
				return "", fmt.Errorf("stage_memory: content is empty")
			}
			return "Staged for memory (kept only if the answer passes vetting).", nil
		},
	)
}

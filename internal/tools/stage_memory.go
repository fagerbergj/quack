package tools

import (
	"fmt"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// stageMemoryArgs is one piece of tradecraft the agent wants to remember.
type stageMemoryArgs struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

// newStageMemory builds the stage_memory tool. It is a SINK: it records nothing
// itself — the call (with its args) lands in the worker's session, and the trust
// gate harvests staged candidates from there, committing them only if the answer
// passes vetting. So nothing is ever remembered from a failed answer.
func newStageMemory(_ Deps) (tool.Tool, error) {
	return functiontool.New[stageMemoryArgs, string](
		functiontool.Config{
			Name: "stage_memory",
			Description: "Stage a durable piece of research tradecraft to remember for future, unrelated tasks — " +
				"e.g. a source that proved authoritative (and for what), a source that was junk, a search/fetch " +
				"tactic that worked, or an availability dead-end (information that simply isn't published). " +
				"`content` is one atomic sentence; `kind` is one of source|search|fetch|deadend. Do NOT stage " +
				"volatile facts (prices, hours) or request-specific answers. Staged items are reviewed and kept " +
				"only if your answer passes vetting — nothing is remembered until then.",
		},
		func(_ agent.ToolContext, a stageMemoryArgs) (string, error) {
			if strings.TrimSpace(a.Content) == "" {
				return "", fmt.Errorf("stage_memory: content is empty")
			}
			return "Staged for memory (kept only if the answer passes vetting).", nil
		},
	)
}

package tools

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/vetting"
)

type checkMermaidArgs struct {
	Diagram string `json:"diagram" jsonschema:"one mermaid diagram's source, without the surrounding fence"`
}

// newCheckMermaid builds check_mermaid: pre-flight validation against the SAME
// mermaid parser the delivery gate runs (vetting.CheckMermaid), so passing this
// tool means the gate's mermaid_valid criterion will also pass - catch a syntax
// error here instead of burning a whole regenerate-the-answer cycle on it.
func newCheckMermaid(_ Deps) (tool.Tool, error) {
	return functiontool.New[checkMermaidArgs, string](
		functiontool.Config{
			Name:        "check_mermaid",
			Description: "Validate one mermaid diagram's source before including it in your final answer. Returns \"ok\" or a parse error with line/column. Call this on every mermaid diagram before submitting.",
		},
		func(_ agent.Context, a checkMermaidArgs) (string, error) {
			ok, line, col, msg := vetting.CheckMermaid(a.Diagram)
			if ok {
				return "ok", nil
			}
			if line > 0 {
				return fmt.Sprintf("invalid (line %d, column %d): %s", line, col, msg), nil
			}
			return "invalid: " + msg, nil
		},
	)
}

package tools

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/vetting"
)

type checkMermaidArgs struct {
	Diagram string `json:"diagram" jsonschema:"one mermaid diagram's source, without the surrounding fence"`
}

// newCheckMermaid runs the gate's own parser (vetting.CheckMermaid), so a pass here means mermaid_valid passes.
func newCheckMermaid(_ Deps) (tool.Tool, error) {
	return functiontool.New[checkMermaidArgs, string](
		functiontool.Config{
			Name:        "check_mermaid",
			Description: vetting.CheckMermaidDescription,
		},
		func(_ agent.Context, a checkMermaidArgs) (string, error) {
			return vetting.CheckMermaid(a.Diagram), nil
		},
	)
}

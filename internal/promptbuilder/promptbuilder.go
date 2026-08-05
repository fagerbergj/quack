// Package promptbuilder: layered system prompts - Identity, Capabilities, Behaviour, Writing, Grading, Environment.
package promptbuilder

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// writing: shared prose ruleset (Writing layer), adapted from Anbeeld/WRITING.md compact variant.
//
//go:embed writing.md
var writing string

// Agent: assembles layered system prompt for native or ACP agents.
func Agent(name, description string, tools []tool.Tool, skills []*skill.Frontmatter, behaviour, grading, workspace string) string {
	var caps strings.Builder
	if tl := toolLines(tools); tl != "" {
		caps.WriteString("### Tools\n\n")
		caps.WriteString(tl)
	}
	if sl := skillLines(skills); sl != "" {
		if caps.Len() > 0 {
			caps.WriteString("\n")
		}
		caps.WriteString("### Skills\n\n")
		caps.WriteString(sl)
	}
	return layered(fmt.Sprintf("You are Quack's %s. %s", name, description), "Capabilities", caps.String(), behaviour, grading, workspace)
}

// Judge assembles the judge's layered prompt; no Grading layer (judge isn't graded).
func Judge(tools []tool.Tool, behaviour string) string {
	return layered("You are Quack's independent judge. You evaluate another agent's answer for trustworthiness, verifying its claims against a rubric before it reaches the user.", "Tools", toolLines(tools), behaviour, "", "")
}

// Orchestrator assembles the system prompt; no Grading layer (orchestrator isn't a gated DAG node).
func Orchestrator(agentRoster string, skills []*skill.Frontmatter, behaviour string) string {
	var caps strings.Builder
	if r := strings.TrimSpace(agentRoster); r != "" {
		caps.WriteString("### Agents\n\nSpecialist agents you can plan into a DAG (use these exact names):\n\n")
		caps.WriteString(r)
		caps.WriteString("\n")
	}
	if sl := skillLines(skills); sl != "" {
		caps.WriteString("\n### Skills\n\n")
		caps.WriteString(sl)
	}
	return layered("You are Quack's orchestrator. You understand what the user needs, coordinate specialist agents, and apply skills to improve your output before responding.", "Capabilities", caps.String(), behaviour, "", "")
}

// GradingFacts renders the trust-gate contract from resolved vetting.Config; "" when no judge.
func GradingFacts(threshold float64, judgeRounds int, readOnly, requireRetrieval bool) string {
	if judgeRounds <= 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "An independent judge scores your answer against a rubric: every criterion must clear %g, and the LOWEST-scoring criterion decides the verdict (weakest-link - no averaging, no caps). A failing verdict sends its feedback back to you, for up to %d revision round(s).", threshold, judgeRounds)
	if requireRetrieval {
		b.WriteString(" An answer with zero retrieval activity this session fails automatically.")
	}
	if readOnly {
		b.WriteString(" You have no delivery tools here, so completion is the work itself, never a commit or push.")
	}
	return b.String()
}

// layered assembles layers: identity, capabilities, behaviour, writing, grading, environment + workspace.
func layered(identity, capsHeader, capsBody, behaviour, grading, workspaceFacts string) string {
	var sb strings.Builder
	sb.WriteString(identity)
	sb.WriteString("\n")
	if capsBody != "" {
		fmt.Fprintf(&sb, "\n## %s\n\n%s", capsHeader, capsBody)
	}
	if b := strings.TrimSpace(behaviour); b != "" {
		sb.WriteString("\n")
		sb.WriteString(b)
		sb.WriteString("\n")
	}
	if w := strings.TrimSpace(writing); w != "" {
		sb.WriteString("\n")
		sb.WriteString(w)
		sb.WriteString("\n")
	}
	if g := strings.TrimSpace(grading); g != "" {
		fmt.Fprintf(&sb, "\n## Grading\n\n%s\n", g)
	}
	fmt.Fprintf(&sb, "\n## Environment\n\nToday is %s.\n", today())
	if wf := strings.TrimSpace(workspaceFacts); wf != "" {
		sb.WriteString("\n")
		sb.WriteString(wf)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// toolLines: one bullet per tool, or "" if none.
func toolLines(tools []tool.Tool) string {
	var sb strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&sb, "- `%s` - %s\n", t.Name(), t.Description())
	}
	return sb.String()
}

// skillLines: load_skill hint plus one bullet per skill, or "" if none.
func skillLines(skills []*skill.Frontmatter) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Use `load_skill(name)` to load a skill's full instructions before applying it.\n\n")
	for _, s := range skills {
		fmt.Fprintf(&sb, "- `%s` - %s\n", s.Name, s.Description)
	}
	return sb.String()
}

func today() string {
	return time.Now().Format("2006-01-02")
}

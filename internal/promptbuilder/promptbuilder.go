// Package promptbuilder assembles layered system prompts for each agent type.
//
// Every prompt has four layers, ordered from most stable (bottom, best for
// prompt caching) to least stable (top):
//
//  1. Identity  — who this agent is (name + description)
//  2. Capabilities — what it can do (tools for specialist agents; skills for
//     the orchestrator — subagents are omitted because ADK injects them
//     automatically via agentTransferInstructionTemplate)
//  3. Behaviour — how it should behave (the agent's prompt.md)
//  4. Environment — contextual facts injected at startup (current date)
package promptbuilder

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// Agent assembles the 4-layer system prompt for a specialist agent.
// name and description come from agent-card.json; tools from the registered
// tool list; behaviour from the agent's prompt.md.
//
// The tools are also sent as function declarations in the API request, so the
// model already knows what each does; listing them here adds workflow context.
func Agent(name, description string, tools []tool.Tool, behaviour string) string {
	return layered(fmt.Sprintf("You are Quack's %s. %s", name, description), "Tools", toolLines(tools), behaviour)
}

// Judge assembles the layered system prompt for the trust gate's independent
// judge, mirroring Agent's structure so the judge is prompted consistently with
// the agents it evaluates. tools are the judge's verification tools plus
// submit_verdict; behaviour is the judging instructions.
func Judge(tools []tool.Tool, behaviour string) string {
	return layered("You are Quack's independent judge. You evaluate another agent's answer for trustworthiness, verifying its claims against a rubric before it reaches the user.", "Tools", toolLines(tools), behaviour)
}

// Orchestrator assembles the system prompt for the orchestrator. agentRoster is
// the available specialist agents (one "- name — description" line each) — the
// orchestrator authors a DAG over them and submits it to the plan tool; skills
// come from the skills/ filesystem; behaviour from prompt.md.
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
	return layered("You are Quack's orchestrator. You understand what the user needs, coordinate specialist agents, and apply skills to improve your output before responding.", "Capabilities", caps.String(), behaviour)
}

// layered assembles the four prompt layers, ordered most-stable-first for prompt
// caching: identity, an optional capabilities section (## header + body),
// behaviour (prompt.md), and the environment footer.
func layered(identity, capsHeader, capsBody, behaviour string) string {
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
	fmt.Fprintf(&sb, "\n## Environment\n\nToday is %s.\n", today())
	return strings.TrimSpace(sb.String())
}

// toolLines renders one bullet per tool (name + description), or "" if none.
func toolLines(tools []tool.Tool) string {
	var sb strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&sb, "- `%s` — %s\n", t.Name(), t.Description())
	}
	return sb.String()
}

// skillLines renders the load_skill hint plus one bullet per skill, or "" if
// none. ADK does not surface skill names to the model automatically.
func skillLines(skills []*skill.Frontmatter) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Use `load_skill(name)` to load a skill's full instructions before applying it.\n\n")
	for _, s := range skills {
		fmt.Fprintf(&sb, "- `%s` — %s\n", s.Name, s.Description)
	}
	return sb.String()
}

func today() string {
	return time.Now().Format("2006-01-02")
}

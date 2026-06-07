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

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/skilltoolset/skill"
)

// Agent assembles the 4-layer system prompt for a specialist agent.
// name and description come from agent-card.json; tools from the registered
// tool list; behaviour from the agent's prompt.md.
func Agent(name, description string, tools []tool.Tool, behaviour string) string {
	var sb strings.Builder

	// Layer 1: Identity
	fmt.Fprintf(&sb, "You are Quack's %s. %s\n", name, description)

	// Layer 2: Capabilities — tool names and descriptions.
	// ADK sends these as function declarations in the API request, so the model
	// already knows what each tool does; listing them here adds workflow context.
	if len(tools) > 0 {
		sb.WriteString("\n## Tools\n\n")
		for _, t := range tools {
			fmt.Fprintf(&sb, "- `%s` — %s\n", t.Name(), t.Description())
		}
	}

	// Layer 3: Behaviour
	if b := strings.TrimSpace(behaviour); b != "" {
		sb.WriteString("\n")
		sb.WriteString(b)
		sb.WriteString("\n")
	}

	// Layer 4: Environment
	fmt.Fprintf(&sb, "\n## Environment\n\nToday is %s.\n", today())

	return strings.TrimSpace(sb.String())
}

// Orchestrator assembles the 4-layer system prompt for the orchestrator.
// skills come from the skills/ filesystem; behaviour from prompt.md.
//
// Subagents are intentionally omitted from the capabilities layer: ADK
// auto-injects the full agent list and transfer_to_agent tool via
// agentTransferInstructionTemplate when SubAgents are registered on the
// llmagent.Config, so duplicating them here would be redundant.
func Orchestrator(skills []*skill.Frontmatter, behaviour string) string {
	var sb strings.Builder

	// Layer 1: Identity
	sb.WriteString("You are Quack's orchestrator. You understand what the user needs, coordinate specialist agents, and apply skills to improve your output before responding.\n")

	// Layer 2: Capabilities — available skills.
	// ADK does not surface skill names to the model automatically, so we inject
	// them here so the orchestrator knows what exists before deciding to act.
	if len(skills) > 0 {
		sb.WriteString("\n## Skills\n\n")
		sb.WriteString("Use `load_skill(name)` to load a skill's full instructions before applying it.\n\n")
		for _, s := range skills {
			fmt.Fprintf(&sb, "- `%s` — %s\n", s.Name, s.Description)
		}
	}

	// Layer 3: Behaviour
	if b := strings.TrimSpace(behaviour); b != "" {
		sb.WriteString("\n")
		sb.WriteString(b)
		sb.WriteString("\n")
	}

	// Layer 4: Environment
	fmt.Fprintf(&sb, "\n## Environment\n\nToday is %s.\n", today())

	return strings.TrimSpace(sb.String())
}

func today() string {
	return time.Now().Format("2006-01-02")
}

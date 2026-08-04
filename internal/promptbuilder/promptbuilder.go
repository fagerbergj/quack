// Package promptbuilder assembles layered system prompts for each agent type.
//
// Every prompt has six layers, ordered from most stable (bottom, best for
// prompt caching) to least stable (top):
//
//  1. Identity  - who this agent is (name + description)
//  2. Capabilities - what it can do: tools and/or skills for a specialist
//     agent (an ACP agent gets skills only - see Agent - and the orchestrator
//     gets its agent roster plus skills; subagents are omitted from a
//     specialist's own prompt because ADK injects them automatically via
//     agentTransferInstructionTemplate)
//  3. Behaviour - how it should behave (the agent's prompt.md, plus its
//     memory.md when the agent is a memory participant)
//  4. Writing - the shared prose ruleset (writing.md), applied to every agent
//  5. Grading - the trust-gate contract that actually applies to this agent
//     (weakest-link threshold, retrieval/delivery requirements), sourced from
//     vetting.Config so no number is invented in a bundle's prompt.md
//  6. Environment - contextual facts injected at startup (current date, and
//     for a coding agent, the workspace/toolchain block - workspace.PromptBlock,
//     #663)
package promptbuilder

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// writing is the shared prose ruleset injected as the Writing layer of every
// assembled prompt. Adapted from github.com/Anbeeld/WRITING.md (compact
// variant): concrete over generic, plain words, watch for LLM regularity.
//
//go:embed writing.md
var writing string

// Agent assembles the layered system prompt for a specialist agent - native or
// ACP (an external coding agent has no quack tools, so callers pass nil tools;
// toolLines then renders nothing rather than fabricating a list). name and
// description come from agent-card.json; tools from the registered tool list;
// skills from this agent's declared scope; behaviour from prompt.md (plus
// memory.md, appended by the caller); grading from GradingFacts; workspace
// from workspace.PromptBlock - "" for a non-coding agent, which has no clone
// or sandboxed toolchain to state facts about.
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

// Judge assembles the layered system prompt for the trust gate's independent
// judge, mirroring Agent's structure so the judge is prompted consistently with
// the agents it evaluates. tools are the judge's verification tools plus
// submit_verdict; behaviour is the judging instructions. The judge itself is
// never graded, so it carries no Grading layer.
func Judge(tools []tool.Tool, behaviour string) string {
	return layered("You are Quack's independent judge. You evaluate another agent's answer for trustworthiness, verifying its claims against a rubric before it reaches the user.", "Tools", toolLines(tools), behaviour, "", "")
}

// Orchestrator assembles the system prompt for the orchestrator. agentRoster is
// the available specialist agents (one "- name - description" line each) - the
// orchestrator authors a DAG over them and submits it to the plan tool; skills
// come from the skills/ filesystem; behaviour from prompt.md. The orchestrator
// plans work but is not itself a gated DAG node, so it carries no Grading layer.
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

// GradingFacts renders the trust-gate contract that actually applies to one
// agent - sourced from its resolved vetting.Config (threshold, judge rounds,
// retrieval/delivery requirements) so no number is invented here. "" when the
// agent has no independent judge (judgeRounds <= 0 - e.g. media/image readers,
// whose output a text judge cannot score at all).
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

// layered assembles the prompt layers, ordered most-stable-first for prompt
// caching: identity, an optional capabilities section (## header + body),
// behaviour (prompt.md), the writing ruleset, an optional grading section, and
// the environment footer - the date, plus workspaceFacts (workspace.PromptBlock)
// when the caller is a coding agent.
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

// toolLines renders one bullet per tool (name + description), or "" if none.
func toolLines(tools []tool.Tool) string {
	var sb strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&sb, "- `%s` - %s\n", t.Name(), t.Description())
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
		fmt.Fprintf(&sb, "- `%s` - %s\n", s.Name, s.Description)
	}
	return sb.String()
}

func today() string {
	return time.Now().Format("2006-01-02")
}

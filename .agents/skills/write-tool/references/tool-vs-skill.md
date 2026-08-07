# Tool vs. Skill - Decision Reference

Load this file when deciding whether to implement a tool or a skill.

---

## The Core Distinction

**A tool** is an executable function with typed inputs, outputs, and side effects.
When an agent calls a tool, something happens: a database gets queried, an API gets hit, a file gets written.
Tools are the agent's **hands**.

**A skill** is packaged domain expertise encoded as instructions, templates, and scripts.
Skills don't execute code directly - they shape how the agent thinks and approaches problems. Skills are the agent's **training**.

---

## Decision Matrix

| Dimension | Tool | Skill |
|---|---|---|
| **What it provides** | Executable capability | Behavioral expertise |
| **Execution** | Deterministic, typed I/O | LLM-interpreted instructions |
| **Token cost** | High (schema in context) | Low (progressive disclosure) |
| **External access** | Yes - APIs, databases, network | No - runs locally |
| **Portability** | Cross-framework (MCP, function calling) | Primarily Anthropic-centric today |
| **Maintenance** | Centralized server update | Markdown editing |
| **Best for** | External APIs, databases, deterministic ops | Workflows, conventions, domain knowledge |

---

## Use a Tool When

- The operation needs **deterministic execution** - same inputs always produce the same output.
- You need to **interact with external systems** - auth-required services, third-party APIs, databases.
- **Precision is critical** - financial transactions, data writes, user-facing actions.
- The interface has **stable, well-defined contracts** - typed inputs and known output shape.

## Use a Skill When

- You need to teach the agent **how to approach** a domain - workflows, review conventions, decision criteria.
- The knowledge is **internal and organizational** - team conventions, proprietary patterns, internal processes.
- The interface is **fluid or evolving** - a skill pointing to current docs is cheaper to update than a tool schema.
- You want to **reduce token overhead** - one MCP server can expose 90+ tools consuming 50k+ tokens of schemas before the model starts reasoning; skills load at ~100 tokens metadata.

---

## The Combined Pattern

The most effective architectures use both together:
1. **Skills** teach the agent *how* to approach a domain (workflow, conventions, decision criteria)
2. **Tools** give the agent the capability to execute within that domain (APIs, databases, actions)

---

## Sources

- [Agent Skills vs Tools - Arcade.dev](https://www.arcade.dev/blog/what-are-agent-skills-and-tools/)
- [Skills vs MCP Tools - LlamaIndex](https://www.llamaindex.ai/blog/skills-vs-mcp-tools-for-agents-when-to-use-what)
- [Writing effective tools for agents - Anthropic Engineering](https://www.anthropic.com/engineering/writing-tools-for-agents)

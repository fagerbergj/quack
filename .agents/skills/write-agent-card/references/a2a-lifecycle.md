# A2A Agent Card - Discovery & Activation Lifecycle

Load this file when you need to understand how a card flows from publication to skill execution.

---

## End-to-End Lifecycle

1. **Discovery** - A client agent fetches `/.well-known/agent-card.json` via HTTP GET. Only `name`, `description`, and `skills[].description` are read at this stage (~100 tokens per skill).

2. **Schema Validation** - The card is validated against the official A2A JSON schema. Invalid cards are rejected before any routing occurs.

3. **Semantic Routing** - The client LLM matches the user's request against `skills[].description` and `skills[].examples`. This is transformer-based semantic matching, not keyword search. Card quality here is the primary determinant of correct routing.

4. **Skill Activation** - The selected `SKILL.md` loads. Persona, task flow, and constraints become active behavioral guidance for the subordinate agent.

5. **Deep Resource Loading** - If the task requires API references or schemas, the agent loads `references/`, `scripts/`, or `assets/` files on demand via trigger conditions in the SKILL.md.

---

## A2A vs. MCP

These protocols are complementary, not alternatives:

| Protocol | Purpose |
|---|---|
| **A2A** | Agent-to-agent task delegation and discovery. The card is the contract between agents. |
| **MCP** | Subordinate agent's own tool and data access within its execution loop. |

A typical flow: a client agent discovers and delegates to a worker via A2A; the worker uses MCP to access databases, APIs, and file systems while executing its task.

---

## Card Path

The current standard is `/.well-known/agent-card.json`. The older path `/.well-known/agent.json` is supported for backward compatibility.

---

## Sources

- [A2A Protocol Architecture - Tyk.io](https://tyk.io/learning-center/a2a-protocol-architecture-and-technical-specification/)
- [AgentCard Concept - agent2agent.info](https://agent2agent.info/docs/concepts/agentcard/)
- [Deep Dive SKILL.md - A.B. Vijay Kumar](https://abvijaykumar.medium.com/deep-dive-skill-md-part-1-2-09fc9a536996)

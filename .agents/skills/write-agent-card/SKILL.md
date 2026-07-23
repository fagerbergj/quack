---
name: write-agent-card
description: |
  Writes and audits A2A-compliant agent-card.json files: the machine-readable discovery metadata
  that lets other agents find, route to, and invoke an agent's skills. Covers all required and
  optional JSON fields, skill declaration quality (description, examples, tags), capability flags,
  security schemes, and alignment between the card and the agent's runtime SKILL.md.
  Use when creating a new agent, auditing an existing agent-card.json, or adding/modifying skills
  declared in a card.
license: MIT
---

# Write Agent Card - A2A Agent Discovery Metadata

## Checklist (validate before shipping)

- [ ] `name` is the product name, not a technical identifier (`SupportBot Pro`, not `support_agent_v2`)
- [ ] `description` covers: what it does, task categories, integrations, output formats
- [ ] Every `skills[].description` states what the skill does **and** what requests trigger it
- [ ] Every `skills[].examples` has ≥2 diverse, realistic natural-language prompts
- [ ] Every `skills[].tags` has ≥2 descriptive category tags
- [ ] `capabilities.streaming` is only `true` if SSE is actually implemented
- [ ] `securitySchemes` matches what the endpoint actually enforces
- [ ] `supportedInterfaces[].url` is a live, reachable endpoint
- [ ] Card fields align with the runtime SKILL.md (see Alignment Guide below)

---

## Phase 1 - Gather Inputs

Before writing:

- **What does this agent do?** (task categories, not implementation details)
- **What are its discrete skills?** Each skill should be one coherent capability.
- **What natural-language prompts would a user send to trigger each skill?** Get ≥3 examples per skill.
- **What does it consume and produce?** MIME types for `inputModes` / `outputModes`.
- **Does it stream, support push notifications, or track state history?** Sets `capabilities`.
- **How does it authenticate callers?** Sets `securitySchemes`.

---

## Phase 2 - JSON Card Structure

Save at `/.well-known/agent-card.json` (also supported: `/.well-known/agent.json` for backward compatibility).

```json
{
  "name": "[Product Name - not a technical identifier]",
  "description": "[What it does, task categories, integrations, output formats - written for both humans and semantic routing algorithms]",
  "version": "[MAJOR.MINOR.PATCH]",
  "provider": {
    "organization": "[Org Name]",
    "url": "https://[domain]"
  },
  "documentationUrl": "https://[docs-domain]/api",
  "iconUrl": "https://[cdn]/icon-512.png",
  "supportedInterfaces": [
    {
      "url": "https://[api-domain]/a2a/v1",
      "protocolBinding": "[JSONRPC | GRPC | HTTP+JSON]",
      "protocolVersion": "1.0"
    }
  ],
  "capabilities": {
    "streaming": false,
    "pushNotifications": false,
    "stateTransitionHistory": false
  },
  "securitySchemes": {
    "bearerAuth": {
      "type": "http",
      "scheme": "bearer",
      "bearerFormat": "JWT"
    }
  },
  "defaultInputModes": ["text/plain", "application/json"],
  "defaultOutputModes": ["text/plain", "application/json"],
  "skills": [
    {
      "id": "[skill-identifier]",
      "name": "[Human-Readable Skill Name]",
      "description": "[What this skill does. Triggers on requests involving...]",
      "tags": ["category-1", "category-2"],
      "examples": [
        "[Realistic natural-language prompt a user would send]",
        "[Another distinct trigger phrasing]"
      ],
      "inputModes": ["text/plain"],
      "outputModes": ["application/json"]
    }
  ]
}
```

---

## Phase 3 - Field Quality Rules

**`description` (top-level):** Write for both humans and semantic routing. Include what it does, categories of tasks, integrations (GitHub, Postgres, etc.), and output formats. One paragraph.

**`skills[].description`:** The most routing-critical field. Must state (1) what the skill does and (2) what user requests trigger it. "Analyzes pull requests for security vulnerabilities. Triggers on requests involving code review, security audit, or vulnerability scanning."

**`skills[].examples`:** These are matched against user requests by the client LLM - not keyword search, semantic matching. Include diverse phrasings: direct commands, indirect questions, jargon and plain language variants. Minimum 2; aim for 3–5.

**`skills[].tags`:** Enable filtering in agent registries. Use descriptive category terms, not implementation details. `["security", "code-review", "sdlc"]` - not `["python", "ast-parsing"]`.

**`capabilities`:** Only set `streaming: true` if you actually implement SSE. False positives break client retry logic.

---

## Phase 4 - Alignment Guide

Each card field has a direct counterpart in the agent's runtime SKILL.md. Misalignment means the agent is discoverable for tasks it can't actually perform.

| Card Field | SKILL.md Counterpart | Rule |
|---|---|---|
| `skills[].description` | Role & Goal section | Role definition should mirror discovery text exactly |
| `skills[].examples` | Task Flow + Example Output | Card examples teach the router; SKILL.md examples teach the agent |
| `skills[].tags` | Scope / constraint filters | Tags define when the skill activates; embed as trigger conditions |
| `capabilities.streaming` | Output formatting rules | If true, instruct the agent to stream chunk-by-chunk |
| `securitySchemes` | Constraints & Rules | Define credential handling and unauthorized-access behavior |
| `defaultInputModes`/`outputModes` | Input/Output section | Enforce MIME type adherence in output constraints |

For writing the SKILL.md itself, use the `write-agent-skill` skill.

---

## Resources

- See `assets/example-agent-card.json` + `assets/example-skill.md` for a fully realized pair (CodeReview Sentinel).
- Read `references/a2a-lifecycle.md` if you need to understand how discovery, schema validation, semantic routing, and skill activation work end-to-end.

---

*Sources: [agentry.com A2A Agent Card Guide](https://agentry.com/blog/a2a-agent-card-guide), [agent2agent.info AgentCard spec](https://agent2agent.info/docs/concepts/agentcard/), [Anthropic Agent Skills docs](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/overview). Full lifecycle in `references/a2a-lifecycle.md`.*

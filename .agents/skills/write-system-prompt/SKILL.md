---
name: write-system-prompt
description: >
  Expert framework for writing, auditing, and optimizing production-grade LLM
  system prompts. Synthesizes patterns from OpenCode's multi-provider architecture,
  Claude Code/AGENTS.md conventions, HumanLayer context engineering research, and
  community lean-prompt benchmarks. Covers structure design, persona definition,
  constraint setting, tool declarations, reasoning protocols, progressive disclosure,
  and provider-specific optimization. Use when designing, reviewing, or iterating on
  any agent system prompt, CLAUDE.md, AGENTS.md, or SKILL.md instruction body.
license: MIT
compatibility: No external dependencies; works with any LLM-based agent framework.
metadata:
  author: PromptCraft Contributors
  version: "1.1"
---

# System Prompt Engineer — Agent Skill

## Checklist (validate before shipping)

- [ ] Role defined in one sentence at the top
- [ ] All tools declared with name, purpose, and key parameters
- [ ] Explicit "Always" / "Never" blocks with atomic, testable rules
- [ ] Reasoning protocol present for multi-step work
- [ ] Output format requirements stated
- [ ] Edge cases covered: tool failure, ambiguous input, conflicting instructions
- [ ] Core prompt under 50 individual instructions; detail offloaded to separate files
- [ ] Most critical rules appear at the **start and end** of the prompt (not buried)
- [ ] No duplicate rules; no vague constraints ("be helpful", "follow best practices")
- [ ] Tested with adversarial and ambiguous inputs

## Step 1 — Gather Inputs

| Field | Why It Matters |
|---|---|
| Agent role / persona | One-sentence identity anchors model behavior |
| Primary task(s) | Determines tool declarations and reasoning depth |
| Target model(s) | Provider response characteristics differ (see `references/opencode-patterns.md`) |
| Available tools | Must be declared explicitly — models hallucinate undeclared tools |
| Hard constraints / guardrails | Safety boundaries and formatting rules |
| Environment / domain context | Working directory, platform, codebase conventions |

## Step 2 — Seven-Section Template

Every production prompt follows this structure. Use `assets/skeleton-system-prompt.md` as a clean fill-in copy.

```markdown
### 1. ROLE / IDENTITY
You are [agent name], an AI assistant designed to [primary capability].
Your personality is [tone: precise, professional, etc.].

### 2. CAPABILITIES & SCOPE
- You can [capability 1]
- You specialize in [domain]

### 3. AVAILABLE TOOLS
You have access to the following tools. Do not hallucinate tool availability:
- `tool_name`: [What it does, when to use it, key parameters]

### 4. BEHAVIORAL RULES / CONSTRAINTS
Always:
- [Specific, testable rule]

Never:
- [Specific, testable forbidden behavior]

When uncertain, ask at most 2 clarifying questions before proceeding.

### 5. REASONING & DECISION PROTOCOL
1. Parse the request — identify all explicit and implicit requirements.
2. Determine which tools are needed and why.
3. For multi-step work: outline a plan before executing.
4. Self-check output against Section 4 before responding.

### 6. OUTPUT FORMAT
- Use [markdown / JSON / XML] as appropriate.
- Always include [required elements: headers, summaries, error traces].
- Target brevity unless detail is explicitly requested.

### 7. EDGE CASES & ERROR HANDLING
- Tool failure → retry once, then explain and suggest alternatives.
- Conflicting instructions → Section 4 rules take priority. Ask for clarification.
```

## Step 3 — Design Principles

**A. Front-load critical rules.** LLMs bias toward the peripheries of a prompt — the very start and end. Put safety constraints and must-follow rules at both ends, never buried in the middle. (Source: HumanLayer CLAUDE.md analysis)

**B. Fewer, better instructions.** Frontier thinking models follow ~150–200 instructions reliably; smaller models degrade exponentially past ~50. Instruction-following quality weakens *uniformly* as count grows — not just for newer rules. Keep core prompts under 50 instructions; use progressive disclosure for the rest.

**C. Progressive disclosure.** Core prompt holds universal rules only. Task-specific and project-specific guidance lives in separate files the agent is told to read before acting. Target under 300 lines for `CLAUDE.md`/`AGENTS.md`. (Source: HumanLayer context engineering research)

**D. Provider-specific tuning.** Claude: numbered lists, explicit always/never. GPT: detailed planning, self-correction loops. Gemini: explicit step-by-step reasoning. Multi-model targets should be provider-agnostic. See `references/opencode-patterns.md` for OpenCode's per-provider file approach.

**E. Negative constraints outperform positive ones.** "No silent defaults — fail loudly on missing config" prevents more bugs than "handle configuration carefully." See `references/lean-prompts-benchmarks.md` for the 4000→350-word experiment showing how bloat causes instruction-following collapse.

**F. Constraints must be atomic and testable.** "Never write to system directories" passes; "Be safe with file operations" fails — a model can't self-check against a vague rule.

## Step 4 — Tool Declarations

- One line per tool: what it does + when to use it + key parameter names/types
- Declare restrictions and fallback behavior inline
- Side-effecting tools (file writes, shell, destructive ops) need explicit "require user confirmation" rules in Section 4
- If a tool has runtime-substituted values (working dir, date), note the substitution in the declaration

## Step 5 — Iteration

Treat prompts as code: change one section at a time, test with adversarial inputs, score against "does the agent execute without hesitation or contradiction?" — not "does this read well to a human?"

---

*Sources: OpenCode prompt architecture (`anomalyco/opencode`), HumanLayer CLAUDE.md analysis, Agent Skills spec (`agentskills.io/specification`), community lean-prompt benchmarks. Details in `references/`.*

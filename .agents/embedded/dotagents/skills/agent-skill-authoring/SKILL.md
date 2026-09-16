---
name: agent-skill-authoring
description: |
  Authors self-contained agent skills by extracting user workflows, interviewing for intent and edge cases, and designing modular packages
  with progressive disclosure (lean SKILL.md body, on-demand references/), writing step-by-step instructions with gotchas and validation
  loops, structuring directory trees (references/, assets/, scripts/), and enforcing the full spec checklist before deployment.
  Use whenever the user asks to create a new agent skill, extract expertise into a skill package, build a reusable workflow,
  audit an existing skill for compliance, or convert rough notes into a structured SKILL.md.
license: MIT
allowed-tools: [Bash, Read, Write, Edit, Glob]
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
---

# Agent Skill Authoring - Reusable, Modular AI Agent Skills

## How This Skill Works

1. **Interview** to capture intent, scope, inputs/outputs, edge cases, and failure modes.
2. **Design** the directory layout and progressive disclosure plan.
3. **Draft** the SKILL.md body: concise instructions, gotchas, validation loop.
4. **Populate** `references/`, `assets/`, `scripts/` - never cram detail into SKILL.md.
5. **Validate** against the checklist embedded in `assets/skill-template.md` before delivering.

> **Progressive disclosure is non-negotiable.** Metadata (~100 tokens) → Body (<5,000 tokens / <500 lines) → On-demand resources loaded only when referenced with a trigger condition.

---

## Phase 1 - Interview & Capture Intent

Extract the following before writing anything:

- **What task should this skill automate?** (one coherent unit of work - not a kitchen-sink)
- **What are the inputs and outputs?** Include formats, file types, API calls.
- **What edge cases, gotchas, or environment-specific facts does the agent need?**
- **What tools, libraries, or commands must be used?** Note defaults and alternatives.
- **What would cause this skill to fail or produce bad output?** Define "when NOT to use" boundaries.

If you cannot answer the above coherently, return to the user with targeted questions before proceeding.

## When NOT to Use

If the capability requires **deterministic execution, external system access, or typed I/O contracts** - querying a database, hitting an API, writing a file - implement a tool instead. Skills shape how an agent thinks; tools give it hands. Use `write-tool` for that case.

---

## Phase 2 - Design Architecture

### Directory Layout

```
skill-name/
├── SKILL.md              # Required - YAML frontmatter + lean instructions (<500 lines)
├── references/           # On-demand docs; subdirectories fine (see below)
├── assets/               # Static resources: schemas, images, lookup tables, templates
└── scripts/              # Executable code - agent runs without loading source into context
```

Only `SKILL.md` is mandatory. The spec permits additional top-level directories, but **runtimes vary in what they will actually serve** - several expose only `references/`, `assets/`, and `scripts/`, and silently refuse to list or read anything else. A skill using `resources/` or `docs/` can therefore be unloadable through no fault of its content. Stick to the three unless you control every consumer.

**Nesting inside them is fine.** The spec's "keep file references one level deep" targets reference *chains* - SKILL.md → A → B → C, where the agent has to follow a trail to reach the thing it needs. A predictable path pattern is not a chain: if SKILL.md states `references/<topic>/README.md` and the topic is known, the agent jumps straight there in one hop. Depth is not what strands an agent; indirection and unpredictability are. Thirty topics flattened into one directory is the worse outcome. Organize by what the reader is looking for, and state the path pattern in the Resources section so nothing has to be found by exploration.

### Progressive Disclosure Plan

| Tier | Content | When Loaded |
|---|---|---|
| **1 - Metadata** | `name` + `description` | Always, at startup (~100 tokens) |
| **2 - Instructions** | Full SKILL.md body | On activation (target: <500 lines) |
| **3 - Resources** | `references/`, `assets/`, `scripts/` | On demand, when agent hits a trigger condition |

Tell the agent **exactly when** to load each resource. Example: "Read `references/api-errors.md` if the API returns a status code ≥ 400." A generic "see references for details" is not a trigger condition.

---

## Phase 3 - Write SKILL.md Body

### Frontmatter

```yaml
---
name: <lowercase-hyphenated>         # ≤64 chars; must match folder name exactly
description: <what + when to use>    # ≤1,024 chars; include specific trigger keywords
license: MIT                         # Optional
compatibility: <if required>         # Optional; ≤500 chars; omit if no real requirements
metadata:
  author: <handle>
  version: "1.0"
allowed-tools: [Bash, Read, Write]   # Optional/experimental; a YAML list, not a string
---
```

**Naming rules:** lowercase `a-z`, `0-9`, hyphens only; no leading/trailing `-`; no `--`; ≤64 chars; must match the parent directory name exactly.

**Description rules:** Include both *what* it does and *when* to use it with specific keywords. "Helps with PDFs" fails; "Extracts text and tables from PDF files. Use when the user mentions PDFs, forms, or document extraction." passes.

### Required Body Sections

**Overview** - one paragraph: what the skill does and where it stops.

**When to Use** - specific trigger phrases and contexts.

**When NOT to Use** - explicit misfire prevention. Do not skip this section.

**Step-by-Step Procedure** - numbered steps with checkboxes for multi-step workflows. Calibrate prescriptiveness per step:
- Fragile operations: exact commands, "do not modify or add flags."
- Open-ended operations: explain *why* so the agent can adapt.

**Gotchas** - environment-specific facts that defy reasonable assumptions. This is the highest-value content in most skills. Add corrections here whenever an agent makes a mistake during iteration.

**Validation Loop** - instruct the agent to validate its own output: do work → run validator → fix → repeat until clean.

**Resources** - list each file with its trigger condition. One line per resource.

### Instruction Writing Rules

- Assume the agent is smart - only add what it does not already know.
- Provide one default per decision point; mention alternatives briefly.
- Favor procedures over declarations. Teach how to approach a class of problems, not what to produce for one instance.
- Keep detailed schemas, API docs, and multi-page runbooks in `references/` - not inline.
- Scripts belong in `scripts/` so the agent can run them without loading source into context.

---

## Phase 4 - Validate

Use `assets/skill-template.md` - it contains the full pre-submission checklist. Run every item before delivering. If `skills-ref` is available:

```bash
skills-ref validate ./<skill-name>
```

---

## Quack repo - prompt.md gotcha

`internal/promptbuilder/promptbuilder.go` automatically injects tools and identity into every agent's system prompt before the `prompt.md` content runs:

- **Layer 1 (identity):** `"You are Quack's {name}. {description}"` - sourced from `agent-card.json`.
- **Layer 2 (tools):** a `## Tools` block listing each registered tool's name + description. ADK also sends function declarations in the API request.
- **Layer 3 (behaviour):** the `prompt.md` content.
- **Layer 4 (environment):** today's date.

For the orchestrator pattern, ADK auto-injects subagents + `transfer_to_agent` via `agentTransferInstructionTemplate`.

**Rule:** `prompt.md` files in this repo should contain only Layer 3 - behavioural rules, reasoning protocols, and output format. Do not redeclare tools, agents, or identity; they are injected by the runtime and duplicating them creates conflicting instructions.

Verified in: `internal/promptbuilder/promptbuilder.go` (lines 26–53, `Agent` function).

---

## Anti-Patterns

| Anti-Pattern | Problem | Fix |
|---|---|---|
| **Kitchen-skill** | Agent loads irrelevant content every time | One coherent unit of work; use progressive disclosure |
| **Vague description** | Skill never activates | Write as if telling a colleague exactly when to consult this reference |
| **Off-spec directory name** | Many runtimes serve only `references/`, `assets/`, `scripts/` - anything else may be unreadable | Use one of the three; nest topics *inside* them |
| **Reference chains** | SKILL.md → A → B → C; the agent follows a trail instead of loading what it needs | One hop from SKILL.md to the file, via a stated path pattern |
| **Unpredictable layout** | Agent cannot guess where a resource lives, so it explores or gives up | Derivable paths (`references/<topic>/README.md`) + state the pattern in Resources |
| **Scripts inline** | Full source loads into context every time | Put scripts in `scripts/`; agent executes without reading source |
| **Duplicate rules** | Maintenance debt and contradiction risk | Single source of truth per rule |

---

*Sources: [agentskills.io/specification](https://agentskills.io/specification), [agentskills.io best practices](https://agentskills.io/skill-creation/best-practices), [Anthropic best practices](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/best-practices), [Dotzlaw](https://www.dotzlaw.com/insights/claude-skills/). Full spec detail in `references/spec-requirements.md`.*

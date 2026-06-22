---
name: write-tool
description: |
  Designs and implements AI agent tools: executable functions with typed inputs, outputs, and side
  effects that agents call to interact with external systems or perform deterministic operations.
  Covers tool description writing, parameter design, response shaping, naming conventions, security
  constraints, and evaluation. Also covers adding a new builtin tool to this repo (Go implementation
  + registry entry).
  Use when adding a new capability that requires code execution, API access, file I/O, or any
  deterministic operation. Use write-agent-skill instead if the goal is to teach the agent how to
  approach a domain — skills shape thinking, tools take actions.
license: MIT
---

# Write Tool

## When NOT to Use

If you need to teach the agent *how to approach* a domain — workflows, conventions, decision criteria, domain knowledge — write a skill instead using `write-agent-skill`. Tools are the agent's hands; skills are its training. See `references/tool-vs-skill.md` for the decision matrix.

## Checklist (validate before shipping)

- [ ] Description follows `Tool to <what>. Use when <specific trigger>.` pattern
- [ ] Description explicitly states what the tool should **not** be used for
- [ ] Tool name is `snake_case`; one concern per tool (not a do-everything tool)
- [ ] All parameters are strongly typed; enums used for finite sets
- [ ] Conditional parameter requirements are documented in the description
- [ ] Response returns only what the agent needs for its next decision — nothing extra
- [ ] Side-effecting tools (writes, deletes, sends) require explicit user confirmation in the agent's constraints
- [ ] Tool is scoped to least privilege — cannot be coerced into actions beyond its declared scope
- [ ] **Portable by design**: a tool backed by external software talks to it through a port (interface) with a config-selected adapter — never a hardcoded backend
- [ ] Evaluation tasks written before shipping (multi-step, real-world prompts)

---

## Phase 1 — Gather Inputs

Before designing the tool:

- **What action does it perform?** One concrete operation — not a family of operations.
- **What external system or resource does it touch?** Database, API, filesystem, network?
- **What does the agent need from the response to decide its next step?** Shape the output around that.
- **Are there preconditions the agent must satisfy before calling this tool?** Document as constraints.
- **Is this side-effecting?** (writes, deletes, sends, charges) — if so, the calling agent must gate it behind a confirmation step.

---

## Phase 2 — Name and Description

**Naming:** `snake_case` always. One concern per tool — `delete_file` not `manage_file`. Group related tools under a consistent namespace prefix (`github_search_issues`, `github_create_pr`).

**Description formula:**

```
Tool to <what it does>.
Use when <specific situation>.
Do NOT use to <what it must not do>.
```

The "Do NOT use" line is not optional for tools that could be misapplied — Anthropic's internal evaluations found explicit negative constraints significantly improve invocation accuracy.

Keep descriptions under ~200 words. Include:
- Hard limits (row caps, timeout, size limits)
- Required preconditions ("only call after confirming the user's destination and party size")
- Format restrictions on inputs

**Example:**

```
Tool to fetch the raw HTML content of a URL.
Use when the agent needs to read a specific page it found in search results.
Do NOT use to submit forms, log in, or interact with JavaScript-rendered content — only static HTML is returned.
Caps: 500KB max response; times out after 10s.
```

---

## Phase 3 — Parameter Design

- **Strong typing everywhere.** Declare formats: `"format": "email"`, ISO 8601 dates, UUIDs.
- **Enums for finite sets.** If a parameter has valid values, enumerate them — don't accept free strings.
- **Fewer top-level parameters is better.** Each additional parameter increases the chance of a wrong call.
- **Document conditional requirements.** If `json_options` is only required when `format` is `"json"`, state that in the description — the agent cannot see your code.
- **Never require the agent to construct complex nested objects** when a flat parameter list would work.

---

## Phase 4 — Response Design

Every tool response is the agent's next context. Design it to be:

- **Minimal.** Return only what the agent needs to decide its next step. Verbose responses waste context and slow reasoning.
- **Informative.** Include enough for the agent to continue without an extra round-trip. If a result has an ID the agent will need to reference later, include it.
- **Structured for parsing.** For complex outputs, return JSON with predictable keys. For simple outputs, plain text is fine.
- **Honest about failures.** Return a clear error with what went wrong and, if possible, what to try instead. Never return an empty success on failure.

---

## Phase 5 — Security

- **Least privilege.** Scope the tool to exactly what it needs — a read tool cannot be coerced into writing.
- **Validate all inputs server-side.** Schema enforcement alone is not enough; malformed parameters from agents can corrupt data.
- **Flag side effects.** Any tool that writes, deletes, sends, or charges must be declared to the orchestrator as side-effecting. The calling agent's constraints section must require explicit user confirmation before invoking it.

---

## Phase 6 — Adding a Builtin Tool to This Repo

Builtin tools live in `internal/tools/`. To add one:

1. Create `internal/tools/<name>.go` implementing the ADK `tool.Tool` interface. See `internal/tools/fetch.go` or `internal/tools/websearch.go` for the pattern.
2. Register the tool in `internal/tools/registry.go`.
3. Add an entry in `config/quack.yaml` under `tools:` with `kind: builtin`.
4. Bind it to agents in the `agents[]` entries that should have access.
5. Add tests in `internal/tools/<name>_test.go`.

Side-effecting tools should set the appropriate flag so the orchestrator can route confirmation requests through the `confirmation_request` SSE event before execution.

### Portability rule (mandatory for external-backed tools)

A builtin tool that talks to external software (a search engine, a render service,
a vector store, a full-text index, a blob store) **must not** hardcode that
backend. Define a small **port** (interface) the tool depends on, put each
concrete backend in its own **adapter**, and select the adapter with a **kind**
factory driven by config — so swapping the backend is a config change plus one new
adapter, never a rewrite of the tool. Empty kind defaults to the one implemented
adapter, so existing config keeps working.

The web tools are the reference implementation: `WebSearcher` / `PageRenderer`
ports + `newWebSearcher` / `newPageRenderer` factories in
`internal/tools/backends.go`, with the SearXNG and crawl4ai adapters in
`internal/tools/{searxng,crawl4ai}.go`. The pattern mirrors `inference.NewModel`
(provider `kind` → adapter). This keeps the generic logic (SSRF guard, result
shaping, html→markdown) in the tool and the swappable part behind the port.

---

## Phase 7 — Evaluation

Write evaluation tasks before shipping, not after:

- Tasks must require **multiple tool calls** — single-step prompts don't catch selection errors.
- Use **real-world phrasing**, not sanitized test inputs ("Customer 9182 was charged three times — find all relevant log entries").
- Measure: task completion rate, number of tool calls per task, token usage, error rate.
- Review agent transcripts to find confusing descriptions, wrong tool selections, or unnecessary calls — use the model itself to analyze its own traces.
- Tools are never "done" — monitor production errors and iterate.

---

## Resources

- Read `references/tool-vs-skill.md` if you need to decide whether to implement a tool or a skill.
- See `internal/tools/` in this repo for existing builtin tool implementations.

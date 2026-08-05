# Agents and Skills

Quack uses a **specialist agent architecture**: each agent is designed for a specific task type, and they collaborate through a DAG orchestrated by the `orchestrator`.

## Agent Roster

Each agent bundle contains exactly:

| File | Purpose |
|------|---------|
| `agent-card.json` | Metadata: name, description, skills |
| `prompt.md` | System prompt with identity + task instructions |
| `rubric.md` | Judge evaluation criteria (optional) |
| `memory.md` | "What to remember" guidance (optional) |

### Core Agents

| Agent | Purpose | Tools | Skills | Workspace |
|-------|---------|-------|--------|-----------|
| `orchestrator` | Routes requests to specialists; handles simple queries directly | route_request, format_markdown | plan-work, format-markdown, present-coding-plan, contribute | None |
| `web-researcher` | Searches the web and retrieves pages for factual questions | web_search, web_fetch, summarize | research-git-repos | None (no code access) |
| `code-implementer` | Implements code changes in a real repo: clone, read, edit, commit | clone_and_edit, run_checks, commit_change | ponytail (coding discipline) | Read+write git clone |
| `code-reviewer` | Reviews code changes read-only: verify claims, tests, correctness | review_change | ponytail-review | Read-only git clone |
| `code-explorer` | Explores codebases read-only: structure, patterns, implementation traces | explore_repository, trace_implementation | research-git-repos | Read-only git clone |
| `synthesizer` | Merges multiple research findings into one structured answer | synthesize_research | format-markdown | None |

### Specialist Descriptions

#### orchestrator
- **Role**: Task router and conversation endpoint
- **When to use**: Any user-facing request (simple or complex)
- **Flow**: Simple queries → direct answer; complex requests → DAG decomposition → specialist delegation

#### web-researcher
- **Role**: Live web content research with inline citations
- **When to use**: Factual questions about events, prices, products, documentation, comparisons
- **Cannot**: Read source code (route those to code-explorer)

#### code-implementer
- **Role**: Write, fix, refactor code in a real git repository
- **When to use**: Any request to add/modify/remove code
- **Process**: Clone/open repo → read relevant files → make minimal diff → run checks → commit → push
- **Skills**: ponytail (coding discipline submodule)

#### code-reviewer
- **Role**: Review code changes read-only
- **When to use**: PR review, diff analysis, pre-merge verification
- **Process**: Read diff → verify claims → check tests → assess correctness/security/readability → structured verdict
- **Skills**: ponytail-review (read-only)

#### code-explorer
- **Role**: Understand how code works: structure, patterns, implementation traces
- **When to use**: "How does X work?", architecture explanations, codebase analysis
- **Cannot**: Make code changes (route those to code-implementer)

#### synthesizer
- **Role**: Merge multiple research outputs into one cohesive answer
- **When to use**: Multiple web-researcher nodes have completed and their findings need consolidation

## Skills System

Skills are **reusable task libraries** loaded at runtime. Each skill is a directory with:

| File | Purpose |
|------|---------|
| `SKILL.md` | Skill description and usage examples |
| `rubric.md` | Evaluation criteria (optional) |
| `memory.md` | "What to remember" guidance (optional) |

### Built-in Skills

| Skill | Purpose | Used By |
|-------|---------|---------|
| `plan-work` | DAG authoring with node definitions, dependencies, acceptance rubrics | Orchestrator |
| `format-markdown` | Reformat Markdown for clean rendering | Orchestrator, Synthesizer |
| `present-coding-plan` | Format plan replies with markdown structure for GitHub | Orchestrator |
| `contribute` | Issue→plan→agree→implement→review→merge umbrella | Orchestrator |
| `develop-feature` | Feature development workflow | Plan-work delegate |
| `fix-bug` | Bug fixing workflow | Plan-work delegate |
| `review-code` | Code review workflow | Code-reviewer, Plan-work delegate |
| `research-git-repos` | Git repo exploration patterns | Web-researcher, Code-explorer |
| `commit-authoring` | Commit message crafting | Code-implementer |
| `pr-authoring` | PR description drafting | Code-implementer |

### Skill Loading

Skills are loaded from:
1. **`agents/<name>/` bundles** — Agent-local skills
2. **`skills/` directory** — Global skills available to all agents
3. **`.agents/vendor/`** — Vendored skills (e.g., ponytail coding discipline)

## Agent Routing

The orchestrator routes requests based on intent:

```
User Request
    ↓
Orchestrator analyzes intent
    ↓
    ├─→ Simple question → Direct answer (orchestrator + format_markdown)
    ├─→ Web research → web-researcher
    ├─→ Code change → code-implementer
    ├─→ Code review → code-reviewer
    ├─→ Code understanding → code-explorer
    └─→ Multi-part → DAG with specialists
```

## ACP Integration

Some agents run over the **Agent Client Protocol (ACP)**:

| Agent | ACP Mode | Permissions |
|-------|----------|-------------|
| code-implementer | External (`opencode acp`) | Read workspace, write commits |
| code-reviewer | External (`opencode acp`), `read_only: true` | Read-only, no commits |
| code-explorer | External (`opencode acp`) | Read-only, no commits |

**Non-ACP agents** run as native ADK agents:
- orchestrator
- web-researcher
- synthesizer
- advisors

## Memory Management

Agents use scoped memory buckets via `memory_role`:

| Role | Scope | Purpose |
|------|-------|---------|
| `research` | Session-wide | Web-researcher findings |
| `coding` | Session-wide | Code-change findings, user preferences |

Memory is committed only after adversarial vetting passes.

## Extending the Agent Roster

To add a new agent:

1. Create `agents/<name>/` directory
2. Add `agent-card.json` (name, description, skills)
3. Add `prompt.md` with identity + task instructions
4. Add `rubric.md` (judge evaluation criteria)
5. Register in `config/quack.yaml` under `agents.<name>`
6. Add skills if needed in `skills/` or as agent-local

**Never** add unregistered files to agent bundles — CI enforces strict bundle contents.

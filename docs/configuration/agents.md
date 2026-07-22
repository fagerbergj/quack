# Agents

An agent is a **bundle** on disk plus a binding in `config/quack.yaml`. Adding or changing an agent is editing files, not recompiling — see `agent.LoadBundle` / `agent.Build`.

## Bundles

Each bundle lives at `agents/<name>/` and contains exactly:

- `agent-card.json` — the [A2A AgentCard](https://a2a-protocol.org/latest/specification/): identity, description, and the skills it advertises. This is what the orchestrator's planner routes on.
- `prompt.md` — the system prompt. For a native agent this is the whole prompt; for an external ACP agent (below) it's the per-round preamble prepended to the coding agent's own instructions.
- `rubric.md` (optional) — a per-agent judge rubric, overriding `config/rubric.md`.
- `memory.md` (optional) — "what to remember" guidance for shared memory (native agents only).

No other files belong in a bundle — an unrecognized file will get overwritten or ignored.

## Binding a bundle

`config/quack.yaml`'s `agents:` map binds each bundle to a model and, for native agents, an explicit tool list:

```yaml
agents:
  web-researcher:
    bundle: agents/web-researcher
    skills: [format-markdown, research-git-repos]
    provider: default
    model: ${QUACK_RESEARCHER_MODEL}
    context_window: 65536
    memory_role: research
    tools: [web_search, web_fetch, summarize, current_date, load_memory, stage_memory, ask_user, ask_advisor]
```

`tools:` is explicit and independent of the card's `skills` — a skill can come from the model, the prompt, or a tool, so listing tools here is a separate, honest declaration of what the agent can actually reach. `skills:` (a different list — built-in skill names, not the card's A2A skills) names which of quack's own skill library entries this agent may `load_skill`.

`memory_role` buckets the agent into shared memory (`coding` or `research`, empty means no role bucket) — memory is shared by subject, not siloed per agent, so what the code-explorer learns about a repo reaches the code-implementer and the code-reviewer too.

## Native agents vs. external ACP agents

quack runs two different kinds of worker:

**Native (llmagent) agents** — `web-researcher`, `rag-researcher`, `synthesizer`, `media-reader`, `image-reader`, `advisor`, the orchestrator itself — run in-process as ADK `llmagent`s, using the `tools:` list above.

**ACP agents** — `code-implementer`, `code-reviewer`, `code-explorer` — are EXTERNAL subprocesses speaking the [Agent Client Protocol](https://agentclientprotocol.com) (`opencode acp` by default). They carry an `acp:` block instead of a `tools:` list:

```yaml
code-implementer:
  bundle: agents/code-implementer
  provider: default
  model: ${QUACK_CODER_MODEL}
  acp:
    command: ["opencode", "acp"]
    mcp_servers:
      - https://mcp.context7.com/mcp
```

- `command` — the subprocess argv.
- `env` — extra subprocess environment, overriding quack's generated defaults.
- `mcp_servers` — MCP server URLs passed straight through into the subprocess's own MCP config (e.g. context7 for library docs).
- `read_only` — set on `code-reviewer` and `code-explorer`: the agent never commits or pushes, so the gate skips the commit/push delivery demand and instead reads its verdict out of its final answer.

An ACP agent has **no quack tools at all** — it brings its own (opencode's built-in edit/read/shell tools), with the model bound via a generated `OPENCODE_CONFIG_CONTENT` and `git push` denied inside the subprocess (delivery is gate-owned; see [trust-gate.md](trust-gate.md)). quack's skill library is injected via opencode's `skills.paths`, so the same `agents/skills/` content (e.g. the ponytail coding-discipline skills) is available to an ACP worker without it needing quack tool access to read it.

Because the gate can't watch an external subprocess's internals, it reads the ACP agent's *work* off disk instead: `augmentFromRepo` reads the commits/changed files straight off the clone to synthesize a staged PR, and `augmentFromAnswer` parses a reviewer's `VERDICT:`/`FINDINGS:` tail into a staged review with inline comments.

## Skills vs. tools, one more time

The card's `skills` are what the planner sees and routes on — capability-level, honest promises about what the agent as a whole can do. `tools:` in config is what the agent can actually call. They're deliberately decoupled: a translator agent might have a skill ("translate") backed by nothing but its model and prompt, no tools at all.

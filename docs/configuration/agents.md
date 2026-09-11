# Agents

An agent is a **bundle** on disk plus a binding in `config/quack.yaml`. Adding or changing an agent is editing files, not recompiling — see `agent.LoadBundle` / `agent.Build`.

## Bundles

Each bundle lives at `agents/<name>/` and contains exactly:

- `agent-card.json` — the [A2A AgentCard](https://a2a-protocol.org/latest/specification/): identity, description, and the skills it advertises. This is what the orchestrator's planner routes on. An optional `artifact` field names this job's default output kind (one of the registered recordstore artifact kinds) - stamped onto every node assigned this agent, so a node's output is saved as that kind on gate pass without the plan having to say so.
- `prompt.md` — the system prompt. For a native agent this is the whole prompt; for an external ACP agent (below) it's the per-round preamble prepended to the coding agent's own instructions.
- `rubric.yaml` (optional) — a per-agent judge rubric, overriding `config/rubric.md`.
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
    memory:
      bucket: research
    tools: [web_search, web_fetch, summarize, current_date, load_memory, stage_memory, recall_memory, ask_user, ask_advisor]
```

`tools:` is explicit and independent of the card's `skills` — a skill can come from the model, the prompt, or a tool, so listing tools here is a separate, honest declaration of what the agent can actually reach. `skills:` (a different list — built-in skill names, not the card's A2A skills) names which of quack's own skill library entries this agent may `load_skill`.

`memory.bucket` buckets the agent into shared memory (`coding` or `research`, empty/absent means no bucket) — memory is shared by subject, not siloed per agent, so what the code-explorer learns about a repo reaches the code-implementer and the code-reviewer too.

## Native agents vs. external ACP agents

quack runs two different kinds of worker:

**Native (llmagent) agents** — `web-researcher`, `synthesizer`, `media-reader`, `image-reader`, and the orchestrator itself — run in-process as ADK `llmagent`s, using the `tools:` list above.

`agents/advisor` is also a native bundle but isn't bound through the `agents:` map like the others: `internal/serve/serve.go` loads it once at startup, tool-less, bound to the judge's model (not its own), and wires it in as the backing agent for the `ask_advisor` tool a gated worker calls mid-run — it's never dispatched as a node in its own right. See [trust-gate.md](trust-gate.md#the-advisor-is-not-a-gate-stage).

**ACP agents** — `code-implementer`, `code-reviewer`, `code-explorer` — are EXTERNAL subprocesses speaking the [Agent Client Protocol](https://agentclientprotocol.com) (the `tools/pi-acp` shim driving pi, by default). They carry an `acp:` block instead of a `tools:` list:

```yaml
code-implementer:
  bundle: agents/code-implementer
  provider: default
  model: ${QUACK_CODER_MODEL}
  acp:
    command: ["node", "/usr/local/lib/pi-acp/pi-acp.mjs"]
    mcp_servers:
      - https://mcp.context7.com/mcp
```

- `command` — the subprocess argv.
- `env` — extra subprocess environment, overriding quack's generated defaults.
- `mcp_servers` — MCP server URLs; currently has no effect on the ACP subprocess. The pi-acp shim doesn't read it - the only MCP servers that reach pi are quack's own, wired through ACP's `session/new` `mcpServers` (`internal/acp`), not this config.
- `read_only` — set on `code-reviewer` and `code-explorer`: the agent never commits or pushes, so the gate skips the commit/push delivery demand and instead reads its verdict out of its final answer.
- `allow_clone` — set on `code-explorer` only: intended to lift a `git clone`/`gh repo clone` deny so it can shallow-clone a third-party repo into `$TMPDIR` and read it locally. The pi-acp shim enforces no such deny today, so this flag currently has no runtime effect. Requires `read_only`; config validation still rejects setting it without `read_only`.

An ACP agent has **no quack tools at all** — it brings its own (pi's built-in edit/read/shell tools), with the model bound via a generated `PI_ACP_CONFIG` (parsed by the `pi-acp.mjs` shim: endpoint, model, context/output limits, skill paths). `git push` runs with its credentials stripped rather than denied (delivery is gate-owned regardless; see [trust-gate.md](trust-gate.md)). quack's skill library is injected via that same env's skill paths, so the same `agents/skills/` content (e.g. the ponytail coding-discipline skills) is available to an ACP worker without it needing quack tool access to read it.

Each ACP round's preamble also carries a generated workspace/toolchain block: OS, sandbox mode, available toolchains, the `check_commands` allowlist, and the address-space limit, rendered from the resolved workspace caps at startup. Each toolchain line is probed, not just read from config — an unverifiable toolchain gets no line at all, so the agent reports it couldn't verify something instead of trusting a tool that isn't actually reachable.

Because the gate can't watch an external subprocess's internals, it reads the ACP agent's *work* off disk instead: `augmentFromRepo` reads the commits/changed files straight off the clone to synthesize a staged PR, and `augmentFromAnswer` parses a reviewer's `VERDICT:`/`FINDINGS:` tail into a staged review with inline comments.

An ACP agent with a `memory.bucket` set (all three shipped ones have one) also gets `load_memory`/`stage_memory`/`recall_memory` - not because they're named in a `tools:` list (ACP agents have none), but through the same loopback MCP server (`internal/acp/memorymcp.go`) the gate wires up whenever the run is a memory participant. `recall_memory(query, k?)` (epic #1255 P2) queries the agent's own scope on demand mid-run, in addition to the recall already injected into the prompt at the start of the round; every call is logged (a `memory.recall` ledger entry, source `tool`) and its hits join the round's received set, so the judge may vote on them the same way it votes on prefill.

## The orchestrator's tools

The orchestrator stays light on tools — it needs only:

| Tool | Custom? | Description |
| --- | --- | --- |
| `list_nodes` | Yes | Lists this chat's nodes (agent + A2A `context_id`) with status, current assignment, and artifacts - called before authoring a plan, to reuse a node instead of hiring a new one. |
| `create_plan` | Yes | Starts a plan: assignments tying nodes to work (`agent` hires a new node, `node_id` reassigns an existing one), plus `setup`/`delivery`. Returns the plan record for review. |
| `edit_plan` | Yes | Upserts/removes assignments on the chat's current plan by `node_id`, and/or updates `setup`/`delivery`. |
| `execute` | Yes | Runs the plan judge against the current plan record and, if accepted, executes it - a rejection is a tool error naming what to fix. |
| [`agenttool`](https://pkg.go.dev/google.golang.org/adk/tool/agenttool) | No (ADK) | ADK's `AgentTool`, one per discovered agent, to hand a node off to a specialist. |
| [`loadmemorytool`](https://pkg.go.dev/google.golang.org/adk/tool/loadmemorytool) | No (ADK) | The model calls it on demand; routes through quack's `memory.Store` (it implements ADK's `MemoryService`). |
| [`preloadmemorytool`](https://pkg.go.dev/google.golang.org/adk/tool/preloadmemorytool) | No (ADK) | Auto-injects relevant memory into the prompt at the start of each turn. |
| `commit_memory` | Yes | Writes a durable fact to quack's own `memory.Store`; the orchestrator calls it directly for user facts, the gate calls it on a judge pass for task findings. |
| `recall_memory` | Yes | On-demand query into task memory (repo/role/user), scoped to the current chat's user bucket - see [P2's own section below](#recall_memory) for the shape shared with worker nodes and the ACP loopback MCP. |

`commit_memory` relies on the orchestrator model choosing to call it, which doesn't hold up reliably in practice. `orchestrator.user_memory_hook` (#262) is the fix: an end-of-turn hook that, after a cheap keyword pre-filter, hands the message to a dedicated `agents/memory-agent` bundle and commits whatever it extracts - fire-and-forget, so it never affects the response. Off by default (costs a model call per qualifying turn); enable with `orchestrator.user_memory_hook.enabled: true` plus a `provider`/`model`. Its guidance comes from `agents/orchestrator/memory.md` (what's worth remembering) and `agents/memory-agent/rubric.yaml` (the candidate-quality bar) - not duplicated into its own prompt.

This is the orchestrator's own fixed list, not the full builtin tool registry (`internal/tools/` has ~25 tools — web search/fetch, memory, git, filesystem, etc.) that individual agents pull from by name in their own `tools:` list above. That broader registry has no reference doc yet; see the `write-tool` skill for how a builtin tool is defined and registered.

### `recall_memory`

`recall_memory(query, k?)` (epic #1255 P2) is the on-demand twin of the prefill recall every memory-bucketed agent already gets. It's available three ways, all sharing the same contract:

- Native workers and the orchestrator: add `recall_memory` to the agent's `tools:` list (only takes effect when a task-memory store is configured; otherwise it's silently dropped, same as `stage_memory`).
- ACP workers: automatic whenever the run is a memory participant (`memory.bucket` set) - no `tools:` entry, since ACP agents have none; see the ACP section above.

Scope is the caller's own buckets - the same `repo`/`role`/`user` combination the node's prefill recall already uses for a DAG worker, and the user bucket for the orchestrator (repo/role need a resolved workspace, which doesn't exist before a plan runs). `k` narrows the request but never widens it past the store's configured `top_k`; `min_score` applies identically to prefill and this tool, since both read through the same `Store.recall`. A call whose formatted content would exceed the shared injection byte budget returns fewer hits and says so in its own output, rather than silently truncating.

Every call appends a `memory.recall` ledger entry (source `tool`, vs. prefill's `prefill`) and the hits it returned join the round's received set - the judge may vote `supported`/`contradicted`/`not_relevant` on them exactly like prefill hits (deduped by id, so a memory recalled both ways is voted on once). The plan judge also gets a "project memory" section built the same way, logged with source `plan_judge` - but it is never voted on, since the plan judge scores plan shape, not delivered work.

## Skills vs. tools

The card's `skills` are what the planner sees and routes on - capability-level, honest promises about what the agent as a whole can do. `tools:` in config is what the agent can actually call. They're deliberately decoupled: a translator agent might have a skill ("translate") backed by nothing but its model and prompt, no tools at all.

## Extending the workflow catalog

`skills/plan-work/SKILL.md` opens with a "Common workflows" table mapping request shapes ("Single information topic", "Write/fix/refactor code in a repo", ...) to DAG shapes - it's the first thing the planner matches a request against. A deployment running agents the shipped table doesn't know about (a document-ingest pipeline, a reMarkable-notes agent, any house-standard node chain) teaches the planner that shape via the top-level `workflows:` key in `config/quack.yaml` (not nested under `skills:` - it binds onto the DAG planner, a different axis from the skill-library `skills.plugins` above), without forking the skill:

```yaml
workflows:
  - name: document-ingest
    trigger: "Ingest a new document (email, upload, or scanned page) into the knowledge base"
    agents: [document-classifier, document-indexer]
    shape: "ONE `document-classifier` node → ONE `document-indexer` node (terminal - writes the classified document to the KB, the artifact the request asked for)"
```

This renders as a new row directly beneath the shipped table, so the planner still reads ONE table:

| Request | DAG shape |
| --- | --- |
| Ingest a new document (email, upload, or scanned page) into the knowledge base | ONE `document-classifier` node → ONE `document-indexer` node (terminal - writes the classified document to the KB, the artifact the request asked for) |

Each shape needs all four fields - `name` (a short id, also the future storage key), `trigger` and `shape` (the table's two columns), and `agents` (every agent name `shape` mentions). A shape missing any of them is dropped with a startup warning naming it; the rest of the catalog still loads. A shape naming an agent that isn't configured under `agents:` fails startup outright, naming both the shape and the missing agent - a plan the executor can't run must never ship. A shape whose `trigger` collides with an existing row (shipped or an earlier custom one) is refused with a warning rather than composed - precedence goes to whichever loaded first, never to "whichever the model reads".

Composition happens once, at server startup, from `internal/workflowcatalog` - the planner always sees the same deterministic table, not a per-plan lookup that can fail or drift mid-run. A deployment with no `workflows:` gets `skills/plan-work/SKILL.md` completely unchanged.

A shape can also carry an optional `nodes:` list - `{id, agent, task, depends_on, rubric}`, with `task` free to use the literal token `{{ask}}` for the dispatching request's own text. When present, a dispatch naming that shape (an extension's `Run.Workflow`) binds straight to that DAG instead of nudging the planner - no LLM call decomposes the request, because the shape never varies. `trigger`/`shape` still render in the table either way, so the shape stays visible to an ordinary chat request too. A malformed `nodes:` list (unknown agent, a dependency cycle, a duplicate id, a node missing `id`/`agent`/`task`) fails startup outright, naming the shape - the same "never ship what the executor can't run" rule as an unconfigured agent above.

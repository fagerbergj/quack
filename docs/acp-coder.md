# Spec: ACP-backed coding agents

Replace quack's native coding inner loop with external coding agents spoken to over
ACP (Agent Client Protocol — JSON-RPC 2.0, newline-delimited, over stdio). quack
stays the orchestrator: DAG planning, trust gate, judge, delivery, and streaming are
unchanged. The worker inside a coder node becomes a subprocess (`opencode acp` first;
`claude-agent-acp` / `gemini --acp` are config swaps later).

## Why

The coding inner loop is commodity; quack's differentiation is orchestration + the
trust gate. Mature harnesses ship diff-aware edit tools, LSP navigation, and context
compaction we would otherwise keep rebuilding (#252, #277, #316, v0.5.2 hang class).
ACP makes "which coder" a config choice: one client implementation drives opencode,
Claude Code, and gemini-cli.

## Scope

**In:**

- A generic ACP client adapter (`internal/acp/`) built on `github.com/coder/acp-go-sdk`
  (schema-generated, full client side; protocol v1).
- A new agent kind: `AgentConfig` gains an `acp` block (command + env). `buildAgents`
  (serve.go) mints an ACP-backed `adkagent.Agent` instead of an llmagent when present —
  the existing `code-implementer` config key just gains `acp:`, so planner routing,
  `DeriveChecks`, the repo chain, and delivery all keep working by name. The agent
  implements `RunNode(ctx, input) iter.Seq2[*session.Event, error]` so it rides the
  existing seams: dagStream translates its session events to SSE, the activity ledger
  reads them, and events flow through the normal yield sink (persistence/replay intact).
  The bundle stays: the card is identity, `prompt.md` becomes a per-round preamble.
- Translation of ACP `session/update` → ADK session events using **quack's tool
  vocabulary** so `activityFromSessionAt` keeps working (see mapping below). Chunk
  deltas ride `Partial` events (streamed live, never persisted); tool call/response
  pairs and the final answer are durable events.
- A **disk-truth git probe** (`vetting.augmentFromRepo`): an external coder commits
  with its own git, invisible to the session ledger — for setup-provisioned nodes the
  gate reads the clone itself (HEAD vs `baseCommit`, `git diff --name-only` for the
  judge's changed-file re-read) and, when the task demands a PR/push, synthesizes the
  staged PR from the commits so `commitDelivery` still posts exactly once, gate-owned.
- Model binding via existing `Provider`/`Model` config: quack generates
  `OPENCODE_CONFIG_CONTENT` (provider `@ai-sdk/openai-compatible`, baseURL from the
  bound provider endpoint, `"permission"` policy with `git push` denied) — no opencode
  config files on disk.
- Lifecycle: **one subprocess per worker round** (draft, continuation, revise each
  spawn → `initialize` → `session/new(cwd)` → `session/prompt` → kill). Revise
  prompts are already self-contained, so no state needs to survive rounds; the repo
  on disk is the shared substrate. Ceiling: round startup cost + re-read context —
  upgrade path is a per-node persistent process.
- Cancellation: on ctx cancel → `session/cancel`, short grace, then kill the
  **process group** (Setpgid + negative-pid kill + WaitDelay — the
  `workspace/exec.go` pattern; covered by a stubborn-agent test).
- Trust gate unchanged: the judge already scores an arbitrary answer string with its
  own jailed read tools; `checksPassCriterion` runs on disk and is diff-appropriate.
- **Gate checks default ON** (bundled fix): `workspace.check_commands` unset now
  defaults to the go/npm/make allowlist, and `deriveChecks` additionally gates each
  candidate on its binary existing on the host (`toolchainPresent`) so the default is
  safe on toolchain-less hosts. Explicit `check_commands: []` still disables. This is
  what makes the gate actually run a repo's tests — for native AND ACP workers alike
  (the checks stage is worker-agnostic: it runs on the clone, not the session).

**Out (explicitly):**

- Client `fs`/`terminal` capabilities — advertise `fs: {false,false}`, `terminal: false`.
  opencode does file I/O directly; the sandbox boundary is the subprocess environment,
  not the protocol.
- ACP `plan` updates, `session/load` resume, MCP server pass-through, modes,
  slash commands, memory tools inside the external coder.
- HITL from inside the ACP agent (no `ask_user` equivalent) — coder nodes already
  run headless.
- claude-agent-acp / gemini-cli integrations (config-only later; claude needs an
  Anthropic-API proxy for local models, gemini needs the Qwen fork).
- Deleting the native coder bundle — happens only after the bakeoff proves ACP wins.

## Forbidden actions

- The ACP agent must **never** push, open PRs, or deliver — delivery stays gate-owned
  (`commitDelivery`). Enforced by: `OPENCODE_PERMISSION` denying `git push*` bash,
  and no `gh`/push credentials in the subprocess env.
- The subprocess must never run outside the node's jail dir: `session/new.cwd` is the
  resolved jail path and the process is launched through the existing workspace
  sandbox (bwrap) confined to it.
- The adapter must never emit SSE through a side channel — all events go through the
  workflow yield so M8 persistence and Last-Event-ID replay see them.
- Never block forever on subprocess exit (v0.5.2 class): every wait has the
  WaitDelay backstop.

## Interfaces

- `internal/acp/` — client adapter: spawn, initialize (protocolVersion 1,
  `clientInfo`), `session/new`, `session/prompt`, `session/update` handler,
  `session/cancel`, kill. Implements `acp.Client` (RequestPermission → auto-allow
  per policy; fs/terminal methods return "unsupported").
- Config: `AgentConfig.Acp { Command []string; Env map[string]string }` in
  `config/config.go`; branch in `buildAgents` (serve.go). Gate `Config` derivation:
  `RequireRetrieval=false`, `ReadOnly=true` (no `git_push` tool).
- Event mapping (`session/update` → ADK session event parts):

  | ACP | ADK part | notes |
  |---|---|---|
  | `agent_thought_chunk` | thought text | → `agent_thinking` |
  | `agent_message_chunk` | text | accumulated → node output / `agent_token` |
  | `tool_call` | `FunctionCall{ID: toolCallId, Name: mapped}` | kind→name: execute→`run_command`, read→`read_file`, edit (diff content)→`edit_file` with `path`, fetch→`web_fetch`, search→`grep`, other→title |
  | `tool_call_update` (completed/failed) | `FunctionResponse{ID: toolCallId}` | create-on-update if `tool_call` never seen; diff `path` feeds `act.written`/`act.paths` |
  | `usage_update` | usage on the run | surfaces on `agent_complete` |
  | unknown variants | skipped | lenient decode, never fatal |

- Prompt delivery: the agent implements `RunNode`, so the prompt arrives as the
  node input (no `emitPrompt` marker needed); final return value is the accumulated
  agent message text (stop reason `end_turn`), error on `refusal`/`max_*`.

## Output contract

A coder node's output is unchanged in shape: the worker's final text (summary of
what was done), with the real artifact being the working tree / branch in the jail
dir, verified by `checksPassCriterion` and the judge's read tools, delivered by the
gate. `node_done.Output`, judge scores, and SSE sequence are identical to native
nodes — the frontend needs zero changes.

## Test cases

1. **Translation round-trip** (fake ACP agent: a test binary speaking ndjson on
   stdio) — emits `agent_thought_chunk("planning")`, `tool_call{id:t1, kind:execute,
   title:"go test"}`, `tool_call_update{id:t1, status:completed}`,
   `agent_message_chunk("done: added handler")`, resolves prompt `end_turn`.
   Assert: session events yield SSE `agent_thinking` → `agent_tool_call(run_command)`
   → `agent_tool_result` (paired by call ID) → node output `"done: added handler"`.
2. **Ledger visibility** — fake agent emits `tool_call{kind:edit}` +
   `tool_call_update{completed, content:[{type:diff, path:"main.go", newText:...}]}`.
   Assert `activityFromSessionAt` lists `main.go` in written paths and the judge
   prompt's changed-files section includes it.
3. **Cancellation** — fake agent never resolves the prompt. Cancel the node.
   Assert: `session/cancel` sent, adapter returns within grace+WaitDelay, the
   subprocess's process group is dead, node ends `node_cancelled` (not hung).
4. **Revise round** — judge fails round 0; assert round 1 sends a second
   `session/prompt` on the same sessionId containing the self-contained revision
   prompt, and a fresh adapter (simulating process death) still succeeds.

## Rollout

1. Adapter + fake-agent tests (no opencode dependency in CI).
2. `agents/coder-acp/` config bound to opencode + llama-swap qwen3-coder on
   jason-server; bakeoff vs native coder and goose on a #252-style task
   (wall-time, tokens, judge pass).
3. If ACP wins: flip the default coder binding; native coder bundle deleted in a
   follow-up.

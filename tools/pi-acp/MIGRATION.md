# Migration: opencode → pi as the ACP worker

`pi-acp.mjs` is a stdio shim: it speaks the ACP subset quack's executor uses
(internal/acp) and drives `pi --mode rpc` (JSONL on stdin/stdout) as the actual coding
agent. Zero quack Go changes; the agent card swaps the acp command string and everything
else (provider, model, skills, MCP tools) flows through channels quack already emits.

## What's done

- **ACP round**: initialize / session/new / session/prompt / session/cancel, streamed
  session/update notifications (message + thought chunks, tool_call(+update),
  usage_update), turn completion on pi's `agent_settled`. See the subset table below.
- **Model/endpoint**: parsed from `OPENCODE_CONFIG_CONTENT` (serve.go:1021 `opencodeEnv`)
  → a per-run `models.json` (`api: openai-completions`, llama.cpp compat flags) in a temp
  `PI_CODING_AGENT_DIR`.
- **Skills**: `OPENCODE_CONFIG_CONTENT.skills.paths` (serve.go `acpSkillPaths` — quack's
  `skills/` plus plugin roots) → pi `settings.json` `{"skills":[...]}`; pi discovers
  SKILL.md directories recursively under those roots, same shape opencode globs.
- **MCP tool bridge (direct)**: quack hands its loopback server in ACP `session/new`'s
  `mcpServers` (memorymcp.go:110 `memoryMCPServers`, an SSE-inline spec
  `{type:"sse",name:"quackmcp",url:base+"/"+secret}`, gated on the shim advertising
  http/sse MCP caps — which it now does). At session start the shim connects
  (`mcp-client.mjs`, ~60 lines of plain fetch against quack's streamable-HTTP handler),
  does `tools/list`, and generates a pi extension into `PI_CODING_AGENT_DIR/extensions/`:
  - `quackmcp.json` — the fetched tool list + URL,
  - `quackmcp.ts` — registers each tool natively via `pi.registerTool()` as
    `quackmcp_<name>` (matching what quack's preamble asserts — acp.go `mcpToolNames`,
    toolnaming_test.go) and proxies `execute` straight to quack's endpoint.

  pi never knows MCP exists: it sees plain registered tools. MCP framing survives only
  on the shim↔quack wire because that's the surface quack already exposes. Follow-up if
  Go surface ever opens: the same vetting.MemSession tools could be exposed as a plain
  HTTP POST-per-tool endpoint and the bridge would lose the MCP handshake entirely.
- **usage_update**: pi's `message_update.usage.totalTokens` (cumulative,
  provider-reported) → ACP `usage_update.used`; asserted in both test legs so quack's
  token metrics can't silently go dark again.
- **Dockerfile**: opencode stage (170MB binary) removed; `pi` stage npm-installs
  `@earendil-works/pi-coding-agent@0.84.2` (145MB node_modules) and the runtime copies it
  plus the two shim files. node was already in the image (mermaid validator), so the
  **net image delta is −25MB**.

## The ACP subset quack actually uses

| ACP surface | Where quack uses it |
|---|---|
| `initialize` (protocolVersion 1) | `acp.go:175` — reads `agentCapabilities.mcpCapabilities.{http,sse}` to decide whether to offer the memory MCP server (`memorymcp.go:110`); shim advertises both true |
| `session/new` `{cwd, mcpServers}` | `acp.go:191` — carries the quackmcp server spec the bridge consumes |
| `session/prompt` `{sessionId, prompt:[text]}` | `acp.go:207` — turn completion = this RPC returning; `stopReason == "refusal"` is an error (`acp.go:250`) |
| `session/cancel` notification | `acp.go:296` on ctx cancel or idle timeout → pi `abort` |
| notif `session/update`: `agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`, `usage_update` | `translate.go:40-92`; final answer = message text since last tool call (`translate.go:97`) |
| client method `session/request_permission` | `proc.go:222` → PermissionJudge — raised by the shim's permission bridge (see HITL below) |

## What the model sees: pi vs opencode tool schemas

Drivability of the bridged tools is equivalent-or-better under pi:

- **opencode**: consumes quack's MCP server itself, derives client-side tool entries,
  prefixes `quackmcp_`, and (per #630/#688 history) sometimes renames on its own layer —
  the reason toolnaming_test.go pins the preamble wording. Input schemas pass through
  from the MCP `inputSchema`.
- **pi + bridge**: the shim fetches the same `inputSchema` JSON Schema from `tools/list`
  and hands it verbatim to `pi.registerTool({name: "quackmcp_"+name, parameters:
  inputSchema})`. pi presents registered tools to the LLM as ordinary function
  definitions (same JSON-schema `parameters` block as its built-ins), so the model sees
  exactly the schema quack's Go side declared — one fewer renaming layer than opencode.
  Built-in tools differ in vocabulary (pi: `bash/read/edit/write/grep/find/ls` vs
  opencode's set); the shim maps them onto quack's ledger vocabulary by ACP `kind`
  (execute/read/edit/search/fetch) in `KIND`.

## HITL / permission bridge (done)

Plain pi RPC has no permission callback, but pi extensions do: `pi.on("tool_call",
handler)` is the documented interception point and may return `{block: true, reason}`
(extensions.md marks it `tool_call (can block)` in the lifecycle). The generated
extension now guards every tool call:

1. `checkPolicy(toolName, input)` (`mcp-client.mjs`) is the pi translation of the
   opencode permission config quack generates (serve.go:1041): `git push` / `git clone` /
   `gh repo clone` bash commands are **denied locally** — blocked with a reason, no ACP
   round-trip, because delivery is gate-owned. `.env` / `.env.*` reads escalate as asks.
2. An ask POSTs `{toolName, title, input}` to a loopback HTTP endpoint the shim opens
   before spawning pi (port in `quackmcp.json`; stdout is unusable — pi owns it for RPC).
3. The shim raises ACP `session/request_permission` (allow_once/reject_once options) on
   the already-open connection; quack's clientHandler routes it to the safety judge
   (proc.go:222). Deny (or a failed ask) becomes `{block: true, reason}` in pi.

Tested in the fake leg: git push refused with zero permission requests observed; two
`.env` reads raise asks — the judge-allowed one completes, the rejected one fails.

## Observability

Status quo: under opencode, worker LLM calls were invisible to Langfuse (ACP node token
counts were zero). The shim's `usage_update` already fixes the counts — quack's
translator (translate.go:88) turns pi's cumulative `usage.totalTokens` into the node's
UsageMetadata, so per-node totals now land in the ledger and Langfuse.

Per-LLM-call generations were investigated, not implemented:

- **pi native (route 1): not a config win.** `@earendil-works/pi-telemetry` is
  contracts-only — an explicit `TelemetryContext`/`TelemetrySpan` interface with "no
  exporter, no global state, no backend dependency" (its README's words). `pi-agent` and
  `pi-ai` accept a context, but the coding-agent CLI never wires one and greps clean of
  OTLP/OTEL/opentelemetry: there is no env or config that makes `pi --mode rpc` emit
  spans. Native telemetry would mean re-basing the shim on the SDK (`AgentSession` +
  a custom adapter) instead of RPC — not worth it for this.
- **Shim-side OTLP (route 2): the viable follow-up.** The shim already sees everything a
  generation span needs: model (from `OPENCODE_CONFIG_CONTENT`), `message_start`/
  `message_end` boundaries, per-update cumulative usage, tool events. Emitting
  OTLP/HTTP JSON to `http://otel-collector:4318/v1/traces` is plain fetch, ~80 lines, no
  deps. The one missing piece is trace parentage: nothing quack sends into the
  subprocess carries a trace id — `session/new` params are only `{cwd, mcpServers}`
  (acp.go:191) and `spawnEnv` (proc.go:60) sets no OTEL vars. **The one Go-side change
  worth making**: put the round span's W3C `TRACEPARENT` into the child env in
  `startLive` (proc.go, one line with `ctx` already in hand); the shim forwards it as
  the parent of its generation spans.

**Recommendation**: ship with `usage_update` only (per-node totals restored — already
better than opencode). Add the shim-side OTLP emitter plus the one-line TRACEPARENT env
when per-call generations are wanted; Langfuse would then show, under each quack.run
review node: one generation span per pi LLM call with model, input/output/total tokens,
and latency, plus the tool executions between them.

## Still stubbed / accepted losses

- Refusal stop reason: pi has no refusal signal; shim always reports `end_turn`.
- Edit fidelity: no ACP diff blocks; pi edits land as `write_file {path}` in the ledger
  (quack's `edit_file` mapping wants a diff — translate.go:122).
- Directory-escape asks: checkPolicy covers the deny-list and `.env` reads; escapes
  outside cwd rely on the bwrap sandbox (`workspace.WrapArgv`), which applies unchanged.
- Skills assertion covers the settings plumbing, not pi's discovery of a real SKILL.md
  tree (the test path doesn't exist locally; pi ignores missing roots).
- Hermetic child PATH (proc.go:60): `node` and `pi` must be on ChildPath/Caps.ExtraPath.

## Footprint (measured, same stub endpoint, one turn)

| | opencode | pi + shim |
|---|---|---|
| in image | 170 MB binary | 145 MB node_modules + ~15 KB shim (node already present) → **net −25 MB** |
| idle RSS | 370 MB | shim 50 MB + pi ~140 MB |
| peak RSS during turn | 582 MB | 198 MB (pi 148 + shim 50) |
| CPU per turn (user+sys) | 6.4 s | 0.35 s (whole tree) |

The MCP bridge adds no measurable RSS/CPU: it is one HTTP handshake + two small files at
session start, inside the same shim process.

## How to validate

```sh
node tools/pi-acp/test.mjs              # scripted: fake pi + fake quackmcp server; asserts
                                        # the full round incl. a bridged quackmcp_stage_review
                                        # call landing on the MCP server, and usage_update
node tools/pi-acp/mock-openai.mjs &     # then, with pi on PATH:
OPENCODE_CONFIG_CONTENT='{"provider":{"quack":{"options":{"baseURL":"http://127.0.0.1:8091/v1","apiKey":"unused"},"models":{"qwen3.8-27b":{}}}},"skills":{"paths":["/opt/quack/skills"]}}' \
  PI_ACP_REAL=1 node tools/pi-acp/test.mjs
```

The real-pi leg drives an actual `pi --mode rpc`: the stub LLM answers with a
`quackmcp_stage_review` tool call, pi executes it through the generated extension, and
the test asserts it landed on the fake quack MCP server.

Against the real llm-swap, swap the baseURL for `http://llm-swap:11436/v1` and the model
for `qwen3.8-27b`.

Remaining release gates (all agents including the implementer migrate in one release;
opencode leaves the image entirely): the #964 validation run (reviewer agent on a known
PR set, verdict/comment quality vs the opencode baseline) and the writer-diff/refusal
cosmetics listed under accepted losses.

Agent card change per agent:

```yaml
agents:
  code-reviewer:
    acp:
      command: ["node", "/usr/local/lib/pi-acp/pi-acp.mjs"]
```

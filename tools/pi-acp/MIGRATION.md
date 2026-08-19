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
| client method `session/request_permission` | `proc.go:222` → PermissionJudge — **not exercised under pi** (see HITL below) |

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

## Gated: HITL / permission asks (implementer only)

Plain pi RPC has no permission callback: pi executes tools without asking the client, so
quack's `session/request_permission` → PermissionJudge tier never fires. Reviewer and
explorer don't need it; **the implementer migration is gated on this**.

Design note for the follow-up (all shim-side, no Go changes):

1. Extend the generated extension with `pi.on("tool_call", handler)` — pi's documented
   interception point, which may return `{block: true, reason}` (extensions.md Quick
   Start shows exactly this for `rm -rf`).
2. The handler POSTs `{toolName, input}` to a tiny loopback HTTP server the *shim* opens
   before spawning pi (port passed to the extension via `quackmcp.json`). This is the
   extension→shim channel; stdout is not usable because pi owns it for RPC events.
3. The shim translates that POST into an ACP `session/request_permission` request to
   quack (the connection is already open; quack's clientHandler routes it to the safety
   judge, proc.go:222) and answers the extension with allow/deny; deny becomes
   `{block: true, reason}`.
4. Scope it like opencode's generated permission config (serve.go:1041): auto-allow
   everything except the deny-listed patterns (`git push`, `git clone`, `.env` reads,
   escapes outside cwd) so the judge only sees the exceptional ask, same as today.

## Still stubbed / accepted losses

- Refusal stop reason: pi has no refusal signal; shim always reports `end_turn`.
- Edit fidelity: no ACP diff blocks; pi edits land as `write_file {path}` in the ledger
  (quack's `edit_file` mapping wants a diff — translate.go:122).
- Permission policy config (serve.go:1041 denies) is not translated — lands with the
  HITL extension above. The bwrap sandbox (`workspace.WrapArgv`) applies unchanged.
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
for `qwen3.8-27b`. **Before any release: run the #964 review experiment** (reviewer agent
on a known PR set) against a pi-backed reviewer and compare verdict/comment quality with
the opencode baseline.

Agent card change per agent:

```yaml
agents:
  code-reviewer:
    acp:
      command: ["node", "/opt/quack/pi-acp/pi-acp.mjs"]
```

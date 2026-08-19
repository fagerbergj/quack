# SPIKE: pi as an ACP worker

`pi-acp.mjs` is a stdio shim: it speaks the ACP subset quack's executor uses and drives
`pi --mode rpc` (JSONL on stdin/stdout) as the actual coding agent. No quack Go changes;
the agent card just swaps the acp command string.

## The ACP subset quack actually uses

All in `internal/acp/` (the executor spawns one subprocess per worker round):

| ACP surface | Where quack uses it |
|---|---|
| `initialize` (protocolVersion 1, empty clientCapabilities) | `acp.go:175` — reads back `agentCapabilities.mcpCapabilities.{http,sse,acp}` to decide whether to offer the memory MCP server (`memorymcp.go:110`) |
| `session/new` `{cwd, mcpServers}` | `acp.go:191` |
| `session/prompt` `{sessionId, prompt:[text]}` | `acp.go:207` — turn completion = this RPC returning; `stopReason == "refusal"` becomes an error (`acp.go:250`), anything else ends the round with the accumulated answer text |
| `session/cancel` notification | `acp.go:296` (`gracefulCancel`) on ctx cancel or idle timeout |
| notif `session/update` → `agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`, `usage_update` | `translate.go:40-92` — tool calls map by `kind` (execute/edit/read/fetch) into quack's tool vocabulary; the final answer is message text accumulated since the last tool call (`translate.go:97`) |
| client method `session/request_permission` | `proc.go:222` — routed to the safety-judge tier (`PermissionJudge`) |
| client fs/terminal methods | refused; capabilities never advertised (`proc.go:247+`) |

Model/endpoint plumbing: `internal/serve/serve.go:1021` (`opencodeEnv`) puts a full opencode
config in `OPENCODE_CONFIG_CONTENT` — provider `quack` with `options.baseURL`/`apiKey` and one
model. The shim parses that same env var, so the existing provider config flows through untouched.

## Mapping to pi RPC

Clean:

- `session/prompt` → pi `{"type":"prompt","message":...}`; turn done on pi's `agent_settled` event.
- `message_update` `text_delta`/`thinking_delta` → `agent_message_chunk`/`agent_thought_chunk`.
- `tool_execution_start/end` → `tool_call`/`tool_call_update` (toolCallId correlates; pi tool
  names bash/read/edit/write map onto ACP kinds; pi's `args` pass through as `rawInput`, and
  quack's `firstPath` already reads `path` from rawInput).
- `session/cancel` → pi `{"type":"abort"}`.
- usage: pi's cumulative `usage.totalTokens` → `usage_update.used`.
- Endpoint/model: shim writes a pi `models.json` (provider `quack`, `api: openai-completions`,
  `compat.supportsDeveloperRole/ReasoningEffort: false` for llama.cpp) into a temp
  `PI_CODING_AGENT_DIR`, then `pi --mode rpc --no-session --provider quack --model <id>`.

## Gaps

- **Permission asks / HITL (#963–965 stack)**: plain pi RPC has no permission callback — pi
  executes tools without asking the client. `session/request_permission` → PermissionJudge is
  simply never exercised. Recovering it needs a pi *extension* that intercepts tool calls and
  round-trips an approval over the RPC extension-UI channel. This is the one real gap.
  (Conversely pi's `steer`/`follow_up` are *richer* than ACP's prompt turns — mid-turn steering
  exists natively — but quack's ACP subset has no verb for it, so the shim doesn't expose it.)
- **Memory MCP**: shim advertises `mcpCapabilities` all-false, so quack skips `mcpServers`
  (`memorymcp.go:110`) — load_memory/stage_memory silently off. pi does support MCP-ish custom
  tools via extensions; wiring quack's loopback MCP would be extension work too.
- **Refusal**: pi has no refusal stop reason; shim always reports `end_turn`.
- **Edit fidelity**: quack's `edit_file` mapping wants an ACP diff block (`translate.go:122`);
  the shim sends rawInput only, so pi edits land as `write_file {path}` in the ledger.
- **Permission policy**: opencode's config-level denies (`git push`, `.env` reads — serve.go:1041)
  have no shim-side equivalent; pi has its own settings but the shim doesn't translate them.
  The bwrap sandbox (`workspace.WrapArgv`) still applies unchanged — the shim is just argv.
- **PATH**: the child env is hermetic (`proc.go:60`); `node` must be reachable via Caps.ExtraPath.

## Footprint (measured)

Image: Dockerfile:49 ships opencode v1.18.3 as one self-contained binary. pi is a plain npm
package — the debian:bookworm-slim runtime has no node, so a node runtime must be added.

| | opencode | pi + shim |
|---|---|---|
| on-disk in image | 170 MB (binary; 57 MB tarball layer) | 145 MB node_modules + 118 MB node binary + ~10 KB shim ≈ 263 MB |
| net image delta | — | **+93 MB** if opencode removed |
| idle RSS | 370 MB | shim 50 MB (pi child ~140 MB once a session starts) |
| peak RSS during one turn | 582 MB | 148 MB (pi) + 50 MB (shim) = 198 MB |
| CPU per turn (user+sys, same stub endpoint, 1 canned completion) | 6.4 s | 0.35 s (whole tree) |

Measured on the dev box: `/usr/bin/time -v` over the scripted ACP client plus 100 ms `ps` RSS
sampling of the process tree; both agents drove the identical `mock-openai.mjs` stub.

## Trying it

Agent card / config change (only the command string; provider/model/env untouched — the shim
reuses the `OPENCODE_CONFIG_CONTENT` quack already generates):

```yaml
agents:
  code-implementer:
    acp:
      command: ["node", "/opt/quack/tools/pi-acp/pi-acp.mjs"]
```

pi (`@earendil-works/pi-coding-agent`, bin `pi`) must be on the child PATH.

Tests:

```sh
node tools/pi-acp/test.mjs                       # scripted turn against fake-pi.mjs, no network
node tools/pi-acp/mock-openai.mjs &              # then, with pi installed:
OPENCODE_CONFIG_CONTENT='{"provider":{"quack":{"options":{"baseURL":"http://127.0.0.1:8091/v1","apiKey":"unused"},"models":{"qwen3.8-27b":{}}}}}' \
  PI_ACP_REAL=1 node tools/pi-acp/test.mjs
```

Against the real llm-swap (from a box that can reach it):

```sh
OPENCODE_CONFIG_CONTENT='{"provider":{"quack":{"options":{"baseURL":"http://llm-swap:11436/v1","apiKey":"unused"},"models":{"qwen3.8-27b":{}}}}}' \
  PI_ACP_REAL=1 node tools/pi-acp/test.mjs
```

## Judgment

Keep the shim. It is ~160 lines, needs zero Go changes, and the whole ACP subset quack uses maps
onto pi RPC except permission asks. A native Go pi-RPC driver would duplicate `internal/acp`'s
round/translator machinery for one agent and still face the same permission-ask gap, because that
gap is in pi, not in the transport. If pi graduates past a spike, spend the effort on a pi
extension for tool-approval round-trips (restoring the judge tier), not on a native driver.

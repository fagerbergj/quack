#!/usr/bin/env node
// pi-acp: SPIKE shim speaking the ACP subset quack uses (internal/acp) on
// stdio, driving pig-quack (PIG with quackgate fused in) in `--mode rpc` underneath.
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { createInterface } from "node:readline";
import { writeFileSync, mkdirSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { McpClient } from "./mcp-client.mjs";
import { Otel } from "./otel.mjs";

// Model/endpoint/skills/allow_clone come from PI_ACP_CONFIG, the flat env quack already
// generates for every ACP agent (serve.go piACPEnv) - zero Go changes needed here.
const cfg = process.env.PI_ACP_CONFIG
  ? JSON.parse(process.env.PI_ACP_CONFIG)
  : {};
const prov = cfg.model
  ? { baseUrl: cfg.endpoint, apiKey: cfg.api_key || "unused", model: cfg.model, contextWindow: cfg.context_window, maxTokens: cfg.max_output_tokens, routing: cfg.openrouter_routing }
  : null;

// PI_ACP_STATE_DIR (quack: Jail.ACPStateDir) keeps pi's session files out of
// TMPDIR - the round's own documented scratch space, not a session store.
const stateRoot = process.env.PI_ACP_STATE_DIR || tmpdir();

// How long session/new waits for the fused gate's /ready ping; quack's own start timeout is 60s.
const GATE_READY_MS = 20000;
let gateReported = null;

// pi config+session dir, keyed by ACP session id (not mkdtemp'd) so a fresh
// shim process on session/load finds the same dir session/new wrote.
function piDirFor(sessionId) {
  return join(stateRoot, "pi-acp-" + sessionId);
}

// hasExistingSession: pi 1.0.0 writes "<timestamp>_<sessionId>.jsonl" flat
// into --session-dir - checked before spawning so a stale id fails the load.
function hasExistingSession(sessionId) {
  try {
    return readdirSync(join(piDirFor(sessionId), "pi-sessions")).some((f) => f.endsWith("_" + sessionId + ".jsonl"));
  } catch {
    return false;
  }
}

let piDir;
function ensurePiDir(sessionId) {
  const dir = piDirFor(sessionId);
  mkdirSync(dir, { recursive: true });
  if (prov) {
    const modelEntry = { id: prov.model };
    // omit rather than write 0/undefined: pi's provider-composer rejects
    // contextWindow <= 0 and falls back to its own 128000 default anyway.
    if (prov.contextWindow > 0) modelEntry.contextWindow = prov.contextWindow;
    // unset -> pi's 16384 default, which caps reasoning+answer on the same request.
    if (prov.maxTokens > 0) modelEntry.maxTokens = prov.maxTokens;
    writeFileSync(join(dir, "models.json"), JSON.stringify({
      providers: {
        quack: {
          baseUrl: prov.baseUrl,
          api: "openai-completions",
          apiKey: prov.apiKey,
          // openRouterRouting: pi sends it as the request's `provider` field (OpenRouter upstream pinning).
          compat: { supportsDeveloperRole: false, supportsReasoningEffort: false, ...(prov.routing && { openRouterRouting: prov.routing }) },
          models: [modelEntry],
        },
      },
    }));
  }
  // Skills: same roots the shim gets via skill_paths (serve.go acpSkillPaths).
  if (cfg.skill_paths?.length)
    writeFileSync(join(dir, "settings.json"), JSON.stringify({ skills: cfg.skill_paths }));
  return dir;
}

// Loopback endpoint quackgate POSTs approval-needed tool calls to; the shim escalates them over ACP session/request_permission - quack's
// clientHandler routes the ask to the safety judge (proc.go:222).
const permSrv = createServer((req, res) => {
  if (req.url === "/ready") {
    gateReported?.();
    req.resume();
    return res.writeHead(204).end();
  }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", async () => {
    const { toolName, title, input } = JSON.parse(body);
    let allow = false;
    try {
      const r = await request("session/request_permission", {
        sessionId,
        toolCall: { toolCallId: "perm-" + Math.random().toString(36).slice(2), title, kind: KIND[toolName] || "other", rawInput: input },
        options: [
          { optionId: "allow", name: "Allow", kind: "allow_once" },
          { optionId: "reject", name: "Reject", kind: "reject_once" },
        ],
      });
      allow = r.outcome?.outcome === "selected" && r.outcome.optionId === "allow";
    } catch { /* failed ask = deny */ }
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ allow }));
  });
});

// The MCP bridge + permission guard: quack hands its loopback MCP server in
// session/new mcpServers (memorymcp.go memoryMCPServers, gated on our
// advertised http/sse caps). The shim does the handshake + tools/list and writes
// quackmcp.json, which pig-quack's fused quackgate reads to register and guard the tools.
async function writeExtension(servers) {
  const s = servers?.[0];
  const url = s?.url ?? s?.sse?.url ?? s?.http?.url;
  let tools = [];
  if (url) {
    const client = new McpClient(url);
    await client.connect();
    tools = (await client.toolsList()).tools;
  }
  mcpPrefix = s?.name ?? s?.sse?.name ?? "quackmcp";
  bridgedTools = tools.map((t) => mcpPrefix + "_" + t.name);
  const extDir = join(piDir, "extensions");
  mkdirSync(extDir, { recursive: true });
  writeFileSync(join(extDir, "quackmcp.json"), JSON.stringify({
    url, prefix: mcpPrefix, tools, permPort: permSrv.address().port, allowClone: cfg.allow_clone === true,
  }));
}

// One exporter per shim process; spans parent under quack's round span via
// TRACEPARENT (proc.go traceparentEnv). Endpoint from OTEL_EXPORTER_OTLP_ENDPOINT.
let otel;

const KIND = {
  bash: "execute", read: "read", edit: "edit", write: "edit",
  grep: "search", find: "search", ls: "search", fetch: "fetch",
};

// prefix pi registers quack's bridged MCP tools under (writeExtension sets
// this to the real mcpServers[0].name before pi ever calls one) - stripping
// it recovers the tool's real name, e.g. "quackmcp_write_code_review" ->
// "write_code_review" (internal/acp/translate.go mirrors this convention).
let mcpPrefix = "quackmcp";
let bridgedTools = [];

// mcpKind gives a bridged MCP tool a real ACP kind instead of "other" - a
// heuristic on the name's verb, not the tool's actual side effect, so a
// mis-classified one only costs a wrong icon, never a broken call.
function mcpKind(name) {
  if (/^(read|list|load|recall|get|check)_/.test(name)) return "read";
  if (/^(delete|unstage)_/.test(name)) return "delete";
  return "edit"; // stage/write/edit/save_* - and the safe default for an unrecognized verb
}

const out = (obj) => process.stdout.write(JSON.stringify(obj) + "\n");

// Outbound shim->quack requests (session/request_permission).
const outPending = new Map();
let outId = 0;
function request(method, params) {
  const id = "shim-" + ++outId;
  out({ jsonrpc: "2.0", id, method, params });
  return new Promise((resolve, reject) => outPending.set(id, { resolve, reject }));
}

let pi = null;            // child process
let sessionId = null;
let promptReq = null;     // pending session/prompt JSON-RPC id
let cancelled = false;
// Belt-and-suspenders behind hasExistingSession's pre-check - see startPi's
// stderr watcher and the "session/load did not actually resume" reply below.
let resumeAttempted = false;
let resumeFailed = false;

function notify(update) {
  out({ jsonrpc: "2.0", method: "session/update", params: { sessionId, update } });
}

function textOf(content) {
  return (content || []).filter((c) => c.type === "text").map((c) => c.text).join("");
}

let lastUsage = null;
// pi's last assistant stop this prompt, echoed in the prompt response's _meta: quack's ledger
// keeps the raw response, and a settle alone cannot tell a clean stop from an error or no model call.
let lastStop = null;

function onPiEvent(ev) {
  switch (ev.type) {
    case "message_start":
      if (ev.message?.role === "assistant") otel.genStart();
      break;
    case "message_end":
      if (ev.message?.role === "assistant") {
        otel.genEnd(ev.message, lastUsage);
        lastStop = { pi_stop_reason: ev.message.stopReason, ...(ev.message.errorMessage ? { pi_error: ev.message.errorMessage } : {}) };
        // One usage_update per finished model call: quack records each as an llm.call and sums them per node.
        const u = ev.message.usage;
        if (u?.totalTokens)
          // ACP has no prompt/cached/completion split. pi's own `input` excludes
          // cacheRead/cacheWrite; quack's prompt_tokens must include them (cached is a subset).
          notify({
            sessionUpdate: "usage_update", used: u.totalTokens, size: 0,
            _meta: {
              quack_prompt_tokens: u.input + (u.cacheRead || 0) + (u.cacheWrite || 0),
              quack_cached_tokens: u.cacheRead || 0,
              quack_completion_tokens: u.output,
            },
          });
      }
      break;
    case "message_update": {
      const e = ev.assistantMessageEvent || {};
      if (ev.usage) lastUsage = ev.usage;
      if (e.type === "thinking_delta") otel.addThinking(e.delta);
      if (e.type === "text_delta")
        notify({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: e.delta } });
      else if (e.type === "thinking_delta")
        notify({ sessionUpdate: "agent_thought_chunk", content: { type: "text", text: e.delta } });
      break;
    }
    case "tool_execution_start": {
      otel.toolStart(ev.toolCallId, ev.toolName, ev.args);
      // A bridged MCP tool call (quackmcp_write_code_review, ...) carries no
      // native kind - _meta gives the relay the real name directly rather
      // than making it recover one from the title string (#1278).
      const mcpName = ev.toolName.startsWith(mcpPrefix + "_") ? ev.toolName.slice(mcpPrefix.length + 1) : null;
      notify({
        sessionUpdate: "tool_call", toolCallId: ev.toolCallId,
        title: ev.toolName, kind: mcpName ? mcpKind(mcpName) : (KIND[ev.toolName] || "other"),
        status: "in_progress", rawInput: ev.args || {},
        ...(mcpName ? { _meta: { quack_mcp_tool: mcpName } } : {}),
      });
      break;
    }
    case "tool_execution_end": {
      const txt = textOf(ev.result?.content);
      otel.toolEnd(ev.toolCallId, txt, !!ev.isError);
      notify({
        sessionUpdate: "tool_call_update", toolCallId: ev.toolCallId,
        status: ev.isError ? "failed" : "completed",
        rawOutput: { output: txt },
        content: txt ? [{ type: "content", content: { type: "text", text: txt } }] : [],
      });
      break;
    }
    case "agent_settled":
      otel.flush();
      if (promptReq !== null) {
        if (resumeFailed) {
          out({ jsonrpc: "2.0", id: promptReq, error: { code: -32000, message: "session/load did not actually resume - pi started a blank session" } });
        } else {
          out({ jsonrpc: "2.0", id: promptReq, result: { stopReason: cancelled ? "cancelled" : "end_turn", _meta: lastStop ?? { pi_stop_reason: "none" } } });
        }
        promptReq = null;
        cancelled = false;
        resumeAttempted = false; // only the first prompt after a load is checked
      }
      break;
  }
}

function startPi(cwd, sid) {
  otel = new Otel(prov?.model);
  const cmd = process.env.PI_ACP_PI_CMD || "/usr/local/bin/pig-quack";
  // --session-dir must differ from PI_CODING_AGENT_DIR: equal, pi 1.0.0's
  // session lookup silently misses on the second launch (verified empirically).
  const args = ["--mode", "rpc", "--session-id", sid, "--session-dir", join(piDir, "pi-sessions")];
  if (prov) args.push("--provider", "quack", "--model", prov.model);
  // --tools also filters extension tools, so the bridge's names join pi's four built-ins.
  args.push("--no-extensions", "--tools", ["read", "bash", "edit", "write", ...bridgedTools].join(","));
  const env = { ...process.env, PI_CODING_AGENT_DIR: piDir, PIG_CODING_AGENT_DIR: piDir, PI_OFFLINE: "1" };
  pi = spawn(cmd, args, { cwd, env, stdio: ["pipe", "pipe", "pipe"] });
  pi.on("exit", (code) => {
    if (promptReq !== null)
      out({ jsonrpc: "2.0", id: promptReq, error: { code: -32000, message: `pi exited (${code})` } });
    process.exit(code ?? 1);
  });
  createInterface({ input: pi.stdout }).on("line", (l) => {
    if (!l.trim()) return;
    try { onPiEvent(JSON.parse(l)); } catch { /* non-JSON noise */ }
  });
  // Piped (not "inherit") so a session/load's "no project session found"
  // warning is observable here, not just visible to a human at the console.
  createInterface({ input: pi.stderr }).on("line", (l) => {
    process.stderr.write(l + "\n");
    if (resumeAttempted && l.includes("No project session found")) resumeFailed = true;
  });
}

// startSession is the shared setup behind session/new and session/load: both
// stand up the same per-session piDir, MCP bridge, and pi child.
async function startSession(sid, cwd, mcpServers) {
  piDir = ensurePiDir(sid);
  if (!permSrv.listening) await new Promise((r) => permSrv.listen(0, "127.0.0.1", r));
  await writeExtension(mcpServers).catch((e) => { throw new Error(`mcp bridge: ${e.message}`); });
  const ready = new Promise((resolve, reject) => {
    gateReported = resolve;
    setTimeout(() => reject(new Error(`quackgate did not report in within ${GATE_READY_MS}ms; refusing an ungated engine`)), GATE_READY_MS).unref();
  });
  startPi(cwd, sid);
  // The safety-judge boundary: no session until the engine's gate proves it is subscribed.
  await ready.catch((e) => { pi.removeAllListeners("exit"); pi.kill("SIGKILL"); pi = null; throw e; });
}

async function handle(msg) {
  const reply = (result) => out({ jsonrpc: "2.0", id: msg.id, result });
  const fail = (message) => out({ jsonrpc: "2.0", id: msg.id, error: { code: -32000, message } });
  switch (msg.method) {
    case "initialize":
      reply({
        protocolVersion: 1,
        agentCapabilities: {
          loadSession: true,
          mcpCapabilities: { http: true, sse: true, acp: false },
          promptCapabilities: { audio: false, embeddedContext: false, image: false },
        },
        authMethods: [],
      });
      break;
    case "session/new":
      sessionId = "pi-" + Math.random().toString(36).slice(2);
      try {
        await startSession(sessionId, msg.params.cwd, msg.params.mcpServers);
      } catch (e) {
        return fail(e.message);
      }
      reply({ sessionId });
      break;
    case "session/load":
      // No stdout replay of prior turns on a real resume - quack's round()
      // only waits on this RPC's own response, so that's fine to skip.
      sessionId = msg.params.sessionId;
      if (!hasExistingSession(sessionId)) {
        return fail(`no persisted session for ${sessionId}`);
      }
      resumeAttempted = true;
      try {
        await startSession(sessionId, msg.params.cwd, msg.params.mcpServers);
      } catch (e) {
        return fail(e.message);
      }
      reply({});
      break;
    case "session/prompt": {
      if (!pi) return fail("no live session");
      promptReq = msg.id;
      lastStop = null;
      const text = (msg.params.prompt || []).map((b) => b.text || "").join("");
      pi.stdin.write(JSON.stringify({ type: "prompt", message: text }) + "\n");
      break;
    }
    case "session/cancel":
      cancelled = true;
      pi?.stdin.write(JSON.stringify({ type: "abort" }) + "\n");
      break;
    // _quack/steer (#998): forwards a queued message into pi's native "steer" command.
    case "_quack/steer":
      if (promptReq === null || !pi) { if (msg.id !== undefined) fail("no live round to steer"); break; }
      pi.stdin.write(JSON.stringify({ type: "steer", message: msg.params?.text || "" }) + "\n");
      if (msg.id !== undefined) reply({});
      break;
    default:
      if (msg.id !== undefined)
        out({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: `method not found: ${msg.method}` } });
  }
}

createInterface({ input: process.stdin }).on("line", (l) => {
  if (!l.trim()) return;
  const msg = JSON.parse(l);
  if (msg.method === undefined && outPending.has(msg.id)) {
    const { resolve, reject } = outPending.get(msg.id);
    outPending.delete(msg.id);
    return msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
  }
  handle(msg);
});
process.stdin.on("end", async () => { pi?.kill("SIGKILL"); await otel?.flush(); process.exit(0); });

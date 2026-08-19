#!/usr/bin/env node
// pi-acp: SPIKE shim speaking the ACP subset quack uses (internal/acp) on
// stdio, driving `pi --mode rpc` as the actual coding agent underneath.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Model/endpoint come from OPENCODE_CONFIG_CONTENT, the env quack already
// generates for every ACP agent (serve.go opencodeEnv) - zero Go changes.
function providerFromEnv() {
  const raw = process.env.OPENCODE_CONFIG_CONTENT;
  if (!raw) return null;
  const cfg = JSON.parse(raw);
  const p = cfg.provider?.quack;
  if (!p) return null;
  return {
    baseUrl: p.options.baseURL,
    apiKey: p.options.apiKey || "unused",
    model: Object.keys(p.models)[0],
  };
}

// pi reads models.json from PI_CODING_AGENT_DIR; write our provider there.
function piEnv(prov) {
  const dir = mkdtempSync(join(tmpdir(), "pi-acp-"));
  writeFileSync(join(dir, "models.json"), JSON.stringify({
    providers: {
      quack: {
        baseUrl: prov.baseUrl,
        api: "openai-completions",
        apiKey: prov.apiKey,
        compat: { supportsDeveloperRole: false, supportsReasoningEffort: false },
        models: [{ id: prov.model }],
      },
    },
  }));
  return { ...process.env, PI_CODING_AGENT_DIR: dir };
}

const KIND = {
  bash: "execute", read: "read", edit: "edit", write: "edit",
  grep: "search", find: "search", ls: "search", fetch: "fetch",
};

const out = (obj) => process.stdout.write(JSON.stringify(obj) + "\n");

let pi = null;            // child process
let sessionId = null;
let promptReq = null;     // pending session/prompt JSON-RPC id
let cancelled = false;

function notify(update) {
  out({ jsonrpc: "2.0", method: "session/update", params: { sessionId, update } });
}

function textOf(content) {
  return (content || []).filter((c) => c.type === "text").map((c) => c.text).join("");
}

function onPiEvent(ev) {
  switch (ev.type) {
    case "message_update": {
      const e = ev.assistantMessageEvent || {};
      if (e.type === "text_delta")
        notify({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: e.delta } });
      else if (e.type === "thinking_delta")
        notify({ sessionUpdate: "agent_thought_chunk", content: { type: "text", text: e.delta } });
      if (ev.usage?.totalTokens)
        notify({ sessionUpdate: "usage_update", used: ev.usage.totalTokens, size: 0 });
      break;
    }
    case "tool_execution_start":
      notify({
        sessionUpdate: "tool_call", toolCallId: ev.toolCallId,
        title: ev.toolName, kind: KIND[ev.toolName] || "other",
        status: "in_progress", rawInput: ev.args || {},
      });
      break;
    case "tool_execution_end": {
      const txt = textOf(ev.result?.content);
      notify({
        sessionUpdate: "tool_call_update", toolCallId: ev.toolCallId,
        status: ev.isError ? "failed" : "completed",
        rawOutput: { output: txt },
        content: txt ? [{ type: "content", content: { type: "text", text: txt } }] : [],
      });
      break;
    }
    case "agent_settled":
      if (promptReq !== null) {
        out({ jsonrpc: "2.0", id: promptReq, result: { stopReason: cancelled ? "cancelled" : "end_turn" } });
        promptReq = null;
        cancelled = false;
      }
      break;
  }
}

function startPi(cwd) {
  const prov = providerFromEnv();
  const cmd = process.env.PI_ACP_PI_CMD || "pi";
  const args = process.env.PI_ACP_PI_CMD
    ? []
    : ["--mode", "rpc", "--no-session", "--provider", "quack", "--model", prov.model];
  pi = spawn(cmd, args, { cwd, env: prov ? piEnv(prov) : process.env, stdio: ["pipe", "pipe", "inherit"] });
  pi.on("exit", (code) => {
    if (promptReq !== null)
      out({ jsonrpc: "2.0", id: promptReq, error: { code: -32000, message: `pi exited (${code})` } });
    process.exit(code ?? 1);
  });
  createInterface({ input: pi.stdout }).on("line", (l) => {
    if (!l.trim()) return;
    try { onPiEvent(JSON.parse(l)); } catch { /* non-JSON noise */ }
  });
}

function handle(msg) {
  const reply = (result) => out({ jsonrpc: "2.0", id: msg.id, result });
  switch (msg.method) {
    case "initialize":
      reply({
        protocolVersion: 1,
        agentCapabilities: {
          loadSession: false,
          mcpCapabilities: { http: false, sse: false, acp: false },
          promptCapabilities: { audio: false, embeddedContext: false, image: false },
        },
        authMethods: [],
      });
      break;
    case "session/new":
      sessionId = "pi-" + Math.random().toString(36).slice(2);
      startPi(msg.params.cwd);
      reply({ sessionId });
      break;
    case "session/prompt": {
      promptReq = msg.id;
      const text = (msg.params.prompt || []).map((b) => b.text || "").join("");
      pi.stdin.write(JSON.stringify({ type: "prompt", message: text }) + "\n");
      break;
    }
    case "session/cancel":
      cancelled = true;
      pi?.stdin.write(JSON.stringify({ type: "abort" }) + "\n");
      break;
    default:
      if (msg.id !== undefined)
        out({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: `method not found: ${msg.method}` } });
  }
}

createInterface({ input: process.stdin }).on("line", (l) => {
  if (!l.trim()) return;
  handle(JSON.parse(l));
});
process.stdin.on("end", () => { pi?.kill("SIGKILL"); process.exit(0); });

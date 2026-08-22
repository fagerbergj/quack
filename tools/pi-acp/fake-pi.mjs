#!/usr/bin/env node
// Scripted stand-in for `pi --mode rpc`: replays one canned turn per prompt.
// If the shim generated a quackmcp bridge, calls the first bridged tool and
// runs guarded calls through the SAME policy path the real extension uses.
import { createInterface } from "node:readline";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { McpClient, checkPolicy } from "./mcp-client.mjs";

const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");

function extCfg() {
  try {
    return JSON.parse(readFileSync(join(process.env.PI_CODING_AGENT_DIR, "extensions", "quackmcp.json"), "utf8"));
  } catch {
    return null;
  }
}

async function bridgedCall(cfg) {
  if (!cfg?.tools?.length) return;
  const t = cfg.tools[0];
  const name = cfg.prefix + "_" + t.name;
  out({ type: "tool_execution_start", toolCallId: "call_mcp", toolName: name, args: { verdict: "approve" } });
  const client = new McpClient(cfg.url);
  await client.connect();
  const r = await client.toolsCall(t.name, { verdict: "approve" });
  out({ type: "tool_execution_end", toolCallId: "call_mcp", toolName: name, result: r, isError: !!r.isError });
}

// One tool call through the extension's guard: checkPolicy, then the shim's
// perm endpoint for asks. Emits the tool events pi would.
let seq = 0;
async function guardedCall(cfg, toolName, args) {
  const id = "call_g" + ++seq;
  out({ type: "tool_execution_start", toolCallId: id, toolName, args });
  const v = checkPolicy(toolName, args);
  let blocked = null;
  if (v?.block) blocked = v.block;
  else if (v?.ask) {
    const r = await fetch(`http://127.0.0.1:${cfg.permPort}/perm`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ toolName, title: v.ask, input: args }),
    }).then((x) => x.json());
    if (!r.allow) blocked = "denied by quack safety judge";
  }
  out({
    type: "tool_execution_end", toolCallId: id, toolName,
    result: { content: [{ type: "text", text: blocked ?? "ok" }] }, isError: !!blocked,
  });
}

createInterface({ input: process.stdin }).on("line", async (l) => {
  const msg = JSON.parse(l);
  // Mirrors pi's "steer" command (#998), echoed as a chunk for the test to see.
  if (msg.type === "steer") {
    out({ type: "message_update", assistantMessageEvent: { type: "text_delta", contentIndex: 0, delta: `[steered: ${msg.message}]` } });
    return;
  }
  if (msg.type !== "prompt") return out({ type: "response", command: msg.type, success: true });
  out({ type: "response", command: "prompt", success: true });
  out({ type: "agent_start" });
  out({ type: "message_start", message: { role: "assistant" } });
  out({ type: "message_update", usage: { totalTokens: 42 }, assistantMessageEvent: { type: "thinking_delta", contentIndex: 0, delta: "hmm" } });
  out({ type: "message_end", message: { role: "assistant", content: [{ type: "toolCall", id: "call_1", name: "bash" }], stopReason: "toolUse", usage: { input: 5, output: 1, totalTokens: 42 } } });
  out({ type: "tool_execution_start", toolCallId: "call_1", toolName: "bash", args: { command: "echo hi" } });
  out({ type: "tool_execution_end", toolCallId: "call_1", toolName: "bash", result: { content: [{ type: "text", text: "hi\n" }] }, isError: false });
  const cfg = extCfg();
  await bridgedCall(cfg);
  if (cfg?.permPort) {
    await guardedCall(cfg, "bash", { command: "git push origin main" }); // config-deny, no round-trip
    await guardedCall(cfg, "read", { path: "app/.env" });                // ask -> allow
    await guardedCall(cfg, "read", { path: "app/.env.prod" });           // ask -> deny
  }
  out({ type: "message_start", message: { role: "assistant" } });
  out({ type: "message_update", usage: { totalTokens: 99 }, assistantMessageEvent: { type: "text_delta", contentIndex: 1, delta: "done: hi" } });
  out({ type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "done: hi" }], stopReason: "stop", usage: { input: 10, output: 2, totalTokens: 99 } } });
  out({ type: "agent_end", messages: [], willRetry: false });
  out({ type: "agent_settled" });
});

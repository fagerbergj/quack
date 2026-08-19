#!/usr/bin/env node
// Scripted stand-in for `pi --mode rpc`: replays one canned turn per prompt.
// If the shim generated a quackmcp bridge, calls the first bridged tool the
// same way the real extension does (mcp-client over HTTP), proving the wire.
import { createInterface } from "node:readline";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { McpClient } from "./mcp-client.mjs";

const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");

async function bridgedCall() {
  let cfg;
  try {
    cfg = JSON.parse(readFileSync(join(process.env.PI_CODING_AGENT_DIR, "extensions", "quackmcp.json"), "utf8"));
  } catch {
    return; // no bridge this run
  }
  const t = cfg.tools[0];
  const name = cfg.prefix + "_" + t.name;
  out({ type: "tool_execution_start", toolCallId: "call_mcp", toolName: name, args: { verdict: "approve" } });
  const client = new McpClient(cfg.url);
  await client.connect();
  const r = await client.toolsCall(t.name, { verdict: "approve" });
  out({ type: "tool_execution_end", toolCallId: "call_mcp", toolName: name, result: r, isError: !!r.isError });
}

createInterface({ input: process.stdin }).on("line", async (l) => {
  const msg = JSON.parse(l);
  if (msg.type !== "prompt") return out({ type: "response", command: msg.type, success: true });
  out({ type: "response", command: "prompt", success: true });
  out({ type: "agent_start" });
  out({ type: "message_update", usage: { totalTokens: 42 }, assistantMessageEvent: { type: "thinking_delta", contentIndex: 0, delta: "hmm" } });
  out({ type: "tool_execution_start", toolCallId: "call_1", toolName: "bash", args: { command: "echo hi" } });
  out({ type: "tool_execution_end", toolCallId: "call_1", toolName: "bash", result: { content: [{ type: "text", text: "hi\n" }] }, isError: false });
  await bridgedCall();
  out({ type: "message_update", usage: { totalTokens: 99 }, assistantMessageEvent: { type: "text_delta", contentIndex: 1, delta: "done: hi" } });
  out({ type: "agent_end", messages: [], willRetry: false });
  out({ type: "agent_settled" });
});

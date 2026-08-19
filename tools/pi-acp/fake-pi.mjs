#!/usr/bin/env node
// Scripted stand-in for `pi --mode rpc`: replays one canned turn per prompt.
import { createInterface } from "node:readline";
const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");
createInterface({ input: process.stdin }).on("line", (l) => {
  const msg = JSON.parse(l);
  if (msg.type !== "prompt") return out({ type: "response", command: msg.type, success: true });
  out({ type: "response", command: "prompt", success: true });
  out({ type: "agent_start" });
  out({ type: "message_update", usage: { totalTokens: 42 }, assistantMessageEvent: { type: "thinking_delta", contentIndex: 0, delta: "hmm" } });
  out({ type: "tool_execution_start", toolCallId: "call_1", toolName: "bash", args: { command: "echo hi" } });
  out({ type: "tool_execution_end", toolCallId: "call_1", toolName: "bash", result: { content: [{ type: "text", text: "hi\n" }] }, isError: false });
  out({ type: "message_update", usage: { totalTokens: 99 }, assistantMessageEvent: { type: "text_delta", contentIndex: 1, delta: "done: hi" } });
  out({ type: "agent_end", messages: [], willRetry: false });
  out({ type: "agent_settled" });
});

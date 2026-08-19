#!/usr/bin/env node
// Drives the shim exactly like quack's internal/acp round does:
// initialize -> session/new -> session/prompt, asserting the update stream.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { strict as assert } from "node:assert";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const env = { ...process.env };
if (!process.env.PI_ACP_REAL) env.PI_ACP_PI_CMD = join(here, "fake-pi.mjs");

// ACP_CMD overrides the target, e.g. "opencode acp" for a comparison run.
const argv = process.env.ACP_CMD ? process.env.ACP_CMD.split(" ") : ["node", join(here, "pi-acp.mjs")];
const shim = spawn(argv[0], argv.slice(1), { env, stdio: ["pipe", "pipe", "inherit"] });
const pending = new Map();
const updates = [];
let nextId = 1;

createInterface({ input: shim.stdout }).on("line", (l) => {
  const m = JSON.parse(l);
  if (m.method === "session/update") updates.push(m.params.update);
  else if (m.id !== undefined) {
    const { resolve, reject } = pending.get(m.id);
    m.error ? reject(new Error(m.error.message)) : resolve(m.result);
  }
});

function call(method, params) {
  const id = nextId++;
  shim.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
  return new Promise((resolve, reject) => pending.set(id, { resolve, reject }));
}

const init = await call("initialize", { protocolVersion: 1, clientCapabilities: {} });
assert.equal(init.protocolVersion, 1);
if (!process.env.ACP_CMD) assert.equal(init.agentCapabilities.mcpCapabilities.http, false);

const sess = await call("session/new", { cwd: process.cwd(), mcpServers: [] });
assert.ok(sess.sessionId);

const prompt = process.env.PI_ACP_REAL ? "Reply with exactly: pong" : "do the thing";
const resp = await call("session/prompt", {
  sessionId: sess.sessionId,
  prompt: [{ type: "text", text: prompt }],
});
assert.equal(resp.stopReason, "end_turn");

const kinds = updates.map((u) => u.sessionUpdate);
assert.ok(kinds.includes("agent_message_chunk"), `no message chunk in ${kinds}`);
if (!process.env.PI_ACP_REAL) {
  assert.ok(kinds.includes("agent_thought_chunk"));
  const tc = updates.find((u) => u.sessionUpdate === "tool_call");
  assert.equal(tc.kind, "execute");
  assert.equal(tc.rawInput.command, "echo hi");
  const tu = updates.find((u) => u.sessionUpdate === "tool_call_update");
  assert.equal(tu.status, "completed");
  assert.equal(tu.rawOutput.output, "hi\n");
  assert.ok(updates.some((u) => u.sessionUpdate === "usage_update" && u.used === 99));
}
console.log("ok -", updates.length, "updates, turn completed");
shim.stdin.end();

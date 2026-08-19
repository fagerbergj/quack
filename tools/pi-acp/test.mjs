#!/usr/bin/env node
// Drives the shim exactly like quack's internal/acp round does:
// initialize -> session/new (with a fake quackmcp MCP server) -> session/prompt,
// asserting the update stream and that a bridged quackmcp_* call lands.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { createServer } from "node:http";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { strict as assert } from "node:assert";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";

const here = dirname(fileURLToPath(import.meta.url));
const env = { ...process.env };
if (!process.env.PI_ACP_REAL) env.PI_ACP_PI_CMD = join(here, "fake-pi.mjs");
if (!env.OPENCODE_CONFIG_CONTENT)
  env.OPENCODE_CONFIG_CONTENT = JSON.stringify({
    provider: { quack: { options: { baseURL: "http://127.0.0.1:1/v1", apiKey: "unused" }, models: { stub: {} } } },
    skills: { paths: ["/opt/quack/skills"] },
  });

// Fake quackmcp: minimal MCP streamable-HTTP server, records tools/call.
const mcpCalls = [];
const fakeTools = [
  { name: "stage_review", description: "Stage the final review verdict.", inputSchema: { type: "object", properties: { verdict: { type: "string" } } } },
  { name: "stage_review_comment", description: "Stage one review comment.", inputSchema: { type: "object", properties: { body: { type: "string" } } } },
];
const mcpSrv = createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const m = JSON.parse(body);
    const send = (result) => {
      res.writeHead(200, { "content-type": "application/json", "mcp-session-id": "s1" });
      res.end(JSON.stringify({ jsonrpc: "2.0", id: m.id, result }));
    };
    if (m.method === "initialize")
      send({ protocolVersion: "2025-03-26", capabilities: { tools: {} }, serverInfo: { name: "quackmcp", version: "0" } });
    else if (m.id === undefined) { res.writeHead(202); res.end(); }
    else if (m.method === "tools/list") send({ tools: fakeTools });
    else if (m.method === "tools/call") {
      mcpCalls.push(m.params);
      send({ content: [{ type: "text", text: "staged" }], isError: false });
    } else send({});
  });
});
await new Promise((r) => mcpSrv.listen(0, "127.0.0.1", r));
const mcpUrl = `http://127.0.0.1:${mcpSrv.address().port}/secret123`;

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
if (!process.env.ACP_CMD) assert.equal(init.agentCapabilities.mcpCapabilities.http, true);

const sess = await call("session/new", {
  cwd: process.cwd(),
  mcpServers: [{ type: "sse", name: "quackmcp", url: mcpUrl, headers: [] }],
});
assert.ok(sess.sessionId);

if (!process.env.ACP_CMD) {
  // the shim materialized skills + bridge into the pi config dir
  const piDir = readdirSync(tmpdir()).filter((d) => d.startsWith("pi-acp-"))
    .map((d) => join(tmpdir(), d))
    .filter((d) => { try { readFileSync(join(d, "extensions", "quackmcp.json")); return true; } catch { return false; } })
    .sort((a, b) => statSync(b).mtimeMs - statSync(a).mtimeMs)[0];
  assert.ok(piDir, "no pi config dir with quackmcp bridge found");
  const wantSkills = JSON.parse(env.OPENCODE_CONFIG_CONTENT).skills?.paths;
  if (wantSkills) {
    const settings = JSON.parse(readFileSync(join(piDir, "settings.json"), "utf8"));
    assert.deepEqual(settings.skills, wantSkills);
  }
  const bridge = JSON.parse(readFileSync(join(piDir, "extensions", "quackmcp.json"), "utf8"));
  assert.equal(bridge.prefix, "quackmcp");
  assert.equal(bridge.tools[0].name, "stage_review");
  readFileSync(join(piDir, "extensions", "quackmcp.ts")); // extension generated
}

const prompt = process.env.PI_ACP_REAL
  ? "Call the quackmcp_stage_review tool with verdict approve, then reply done."
  : "do the thing";
const resp = await call("session/prompt", {
  sessionId: sess.sessionId,
  prompt: [{ type: "text", text: prompt }],
});
assert.equal(resp.stopReason, "end_turn");

const kinds = updates.map((u) => u.sessionUpdate);
assert.ok(kinds.includes("agent_message_chunk"), `no message chunk in ${kinds}`);
if (!process.env.ACP_CMD) {
  // the bridged tool call must have landed on the fake MCP server
  assert.equal(mcpCalls.length, 1, `expected 1 mcp call, got ${JSON.stringify(mcpCalls)}`);
  assert.equal(mcpCalls[0].name, "stage_review");
  const btc = updates.find((u) => u.sessionUpdate === "tool_call" && u.title.startsWith("quackmcp_"));
  assert.ok(btc, "no quackmcp_* tool_call update relayed to quack");
  assert.ok(kinds.includes("usage_update"), "usage_update missing - quack metrics would go dark");
}
if (!process.env.PI_ACP_REAL && !process.env.ACP_CMD) {
  assert.ok(kinds.includes("agent_thought_chunk"));
  const tc = updates.find((u) => u.sessionUpdate === "tool_call");
  assert.equal(tc.kind, "execute");
  assert.equal(tc.rawInput.command, "echo hi");
  const tu = updates.find((u) => u.sessionUpdate === "tool_call_update");
  assert.equal(tu.status, "completed");
  assert.equal(tu.rawOutput.output, "hi\n");
  assert.ok(updates.some((u) => u.sessionUpdate === "usage_update" && u.used === 99));
}
console.log("ok -", updates.length, "updates, turn completed,", mcpCalls.length, "mcp call(s) landed");
shim.stdin.end();
mcpSrv.close();

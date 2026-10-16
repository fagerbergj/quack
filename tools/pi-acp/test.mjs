#!/usr/bin/env node
// Real-engine suite: drives quack-acp and the pig-quack under it exactly like quack's internal/acp round does,
// against mock-openai.mjs and a fake quackmcp server. CI runs it inside the built image.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { createServer } from "node:http";
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { strict as assert } from "node:assert";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";

const here = dirname(fileURLToPath(import.meta.url));
const shimCmd = process.env.QUACK_ACP || "/usr/local/bin/quack-acp";

const mock = spawn(process.execPath, [join(here, "mock-openai.mjs")], { env: { ...process.env, PORT: "0" }, stdio: ["ignore", "ignore", "pipe"] });
const port = await new Promise((resolve, reject) => {
  mock.on("exit", (code) => reject(new Error(`mock-openai exited (${code})`)));
  createInterface({ input: mock.stderr }).on("line", (l) => { const m = /on :(\d+)/.exec(l); if (m) resolve(m[1]); });
});
mock.unref();
mock.stderr.destroy();
process.on("exit", () => mock.kill());
const model = `http://127.0.0.1:${port}`;
const config = { endpoint: `${model}/v1`, api_key: "unused", model: "stub", context_window: 65536, max_output_tokens: 32768 };
const env = { ...process.env, PI_ACP_CONFIG: JSON.stringify(config) };
const get = (path) => fetch(model + path).then((r) => r.json());

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
    // A client's GET stream or DELETE on close: this server keeps no session to stream or end.
    if (req.method !== "POST") return res.writeHead(405).end();
    const m = JSON.parse(body);
    const send = (result) => {
      res.writeHead(200, { "content-type": "application/json", "mcp-session-id": "s1" });
      res.end(JSON.stringify({ jsonrpc: "2.0", id: m.id, result }));
    };
    if (m.method === "initialize")
      send({ protocolVersion: "2025-03-26", capabilities: { tools: {} }, serverInfo: { name: "quackmcp", version: "0" } });
    else if (m.id === undefined) res.writeHead(202).end();
    else if (m.method === "tools/list") send({ tools: fakeTools });
    else if (m.method === "tools/call") {
      mcpCalls.push(m.params);
      send({ content: [{ type: "text", text: "staged" }], isError: false });
    } else send({});
  });
});
await new Promise((r) => mcpSrv.listen(0, "127.0.0.1", r));
const mcp = [{ type: "sse", name: "quackmcp", url: `http://127.0.0.1:${mcpSrv.address().port}/secret123`, headers: [] }];

// connectShim: one shim process; decide answers each permission ask.
function connectShim(decide = () => "allow") {
  const s = spawn(shimCmd, [], { env, stdio: ["pipe", "pipe", "inherit"] });
  const pend = new Map();
  let id = 1;
  const updates = [];
  createInterface({ input: s.stdout }).on("line", (l) => {
    if (!l.trim()) return;
    const m = JSON.parse(l);
    if (m.method === "session/update") updates.push(m.params.update);
    else if (m.method === "session/request_permission") {
      s.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: m.id, result: { outcome: { outcome: "selected", optionId: decide() } } }) + "\n");
    } else if (m.id !== undefined && pend.has(m.id)) {
      const { resolve, reject } = pend.get(m.id);
      pend.delete(m.id);
      m.error ? reject(new Error(m.error.message)) : resolve(m.result);
    }
  });
  const call = (method, params) => {
    const rid = id++;
    s.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: rid, method, params }) + "\n");
    return new Promise((resolve, reject) => pend.set(rid, { resolve, reject }));
  };
  const end = async () => {
    s.stdin.end();
    await new Promise((r) => s.on("exit", r));
  };
  return { shim: s, call, updates, end };
}

// Round 1: the bridged MCP tool lands, with its identity and usage relayed.
const a = connectShim();
const init = await a.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
assert.equal(init.protocolVersion, 1);
assert.equal(init.agentCapabilities.mcpCapabilities.http, true);
assert.equal(init.agentCapabilities.loadSession, true, "shim must advertise loadSession for resume to fire");
const sess = await a.call("session/new", { cwd: process.cwd(), mcpServers: mcp });
const piDir = join(tmpdir(), "pi-acp-" + sess.sessionId);
const models = JSON.parse(readFileSync(join(piDir, "models.json"), "utf8"));
assert.equal(models.providers.quack.models[0].contextWindow, 65536, "contextWindow not plumbed from config's context_window");
const resp = await a.call("session/prompt", { sessionId: sess.sessionId, prompt: [{ type: "text", text: "Call the quackmcp_stage_review tool with verdict approve, then reply done." }] });
assert.equal(resp.stopReason, "end_turn");
assert.equal(resp._meta?.pi_stop_reason, "stop", "the last assistant stopReason must ride the prompt response");
const kinds = a.updates.map((u) => u.sessionUpdate);
assert.ok(kinds.includes("agent_message_chunk"), `no message chunk in ${kinds}`);
assert.ok(kinds.includes("usage_update"), "usage_update missing - quack metrics would go dark");
assert.equal(mcpCalls.length, 1, `expected 1 mcp call, got ${JSON.stringify(mcpCalls)}`);
assert.equal(mcpCalls[0].name, "stage_review");
const btc = a.updates.find((u) => u.sessionUpdate === "tool_call" && u.title.startsWith("quackmcp_"));
assert.equal(btc?._meta?.quack_mcp_tool, "stage_review", "MCP identity missing from _meta");
assert.notEqual(btc.kind, "other", "MCP tool call still falls through to kind \"other\"");
await a.end();

// Round 2 on a FRESH shim process must carry round 1's turn into the model's prefix.
const before = await get("/requests");
const b = connectShim();
await b.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
await b.call("session/load", { sessionId: sess.sessionId, cwd: process.cwd(), mcpServers: mcp });
assert.equal((await b.call("session/prompt", { sessionId: sess.sessionId, prompt: [{ type: "text", text: "What did you just do?" }] })).stopReason, "end_turn");
const after = await get("/requests");
assert.ok(after.length > before.length, "round 2 never reached the model");
assert.ok(JSON.stringify(after.at(-1)).includes("Call the quackmcp_stage_review tool"), "round 2's prefix is missing round 1's turn - session/load did not resume");
await b.end();

// A deleted session file must fail session/load, never silently start blank.
const c = connectShim();
await c.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
const sess2 = await c.call("session/new", { cwd: process.cwd(), mcpServers: [] });
await c.call("session/prompt", { sessionId: sess2.sessionId, prompt: [{ type: "text", text: "hi" }] });
await c.end();
const sessionsDir = join(tmpdir(), "pi-acp-" + sess2.sessionId, "pi-sessions");
for (const f of readdirSync(sessionsDir)) rmSync(join(sessionsDir, f));
const d = connectShim();
await d.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
await assert.rejects(d.call("session/load", { sessionId: sess2.sessionId, cwd: process.cwd(), mcpServers: [] }), /no persisted session/);
await d.end();

// The engine's own tool_call gate, usage split, thinking, steer and cancel, driven by mock-openai.mjs SCRIPTS.
const gateCwd = mkdtempSync(join(tmpdir(), "pi-acp-gate-"));
mkdirSync(join(gateCwd, "app"));
writeFileSync(join(gateCwd, "app", ".env"), "A=1\n");
writeFileSync(join(gateCwd, "app", ".env.prod"), "B=2\n");
let asks = 0;
const g = connectShim(() => (++asks === 1 ? "allow" : "reject"));
await g.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
const gs = await g.call("session/new", { cwd: gateCwd, mcpServers: [] });
const say = (text) => g.call("session/prompt", { sessionId: gs.sessionId, prompt: [{ type: "text", text }] });
assert.equal((await say("GATE")).stopReason, "end_turn");
const outFor = (needle) => {
  const tc = g.updates.find((u) => u.sessionUpdate === "tool_call" && JSON.stringify(u.rawInput).includes(needle));
  assert.ok(tc, `no tool_call for ${needle}`);
  return g.updates.find((u) => u.sessionUpdate === "tool_call_update" && u.toolCallId === tc.toolCallId)?.rawOutput?.output ?? "";
};
assert.match(outFor("git push"), /denied by policy/, "engine ran git push");
assert.match(outFor("git clone"), /denied by policy/, "engine ran git clone without allow_clone");
assert.equal(asks, 2, `expected 2 permission asks (the .env reads), got ${asks}`);
assert.match(outFor('"app/.env"'), /A=1/, "judge-allowed .env read did not run");
assert.match(outFor('"app/.env.prod"'), /denied by quack safety judge/, "judge-denied .env read ran");
assert.ok(g.updates.some((u) => u.sessionUpdate === "agent_thought_chunk"), "reasoning_content never surfaced as a thought chunk");
const usage = g.updates.filter((u) => u.sessionUpdate === "usage_update").at(-1);
assert.deepEqual(usage?._meta, { quack_prompt_tokens: 10, quack_cached_tokens: 4, quack_completion_tokens: 2 }, "usage split wrong");

const steered = say("STEER");
await new Promise((r) => setTimeout(r, 500));
await g.call("_quack/steer", { text: "steer-note" });
assert.equal((await steered).stopReason, "end_turn");
assert.ok((await get("/requests")).some((m) => JSON.stringify(m).includes("steer-note")), "steer never reached the model");
// pi's four built-ins plus the bridge: PIG offers more unless the shim limits it.
const tools = (await get("/bodies")).find((x) => x.tools.includes("quackmcp_stage_review")).tools;
assert.deepEqual(tools.toSorted(), ["bash", "edit", "quackmcp_stage_review", "quackmcp_stage_review_comment", "read", "write"], "model offered the wrong tools");

const stalled = say("STALL");
await new Promise((r) => setTimeout(r, 500));
const t0 = Date.now();
g.shim.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "session/cancel", params: { sessionId: gs.sessionId } }) + "\n");
assert.equal((await stalled).stopReason, "cancelled");
const cancelMs = Date.now() - t0;
assert.ok(cancelMs < 3000, `cancel took ${cancelMs}ms`);
assert.equal((await say("still alive?")).stopReason, "end_turn", "session wedged after cancel");
await g.end();
rmSync(gateCwd, { recursive: true });
mcpSrv.close();
console.log(`ok - real engine: bridge, resume, gate, usage, thinking, steer ok; cancel settled in ${cancelMs}ms`);

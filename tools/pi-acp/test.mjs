#!/usr/bin/env node
// Drives the shim exactly like quack's internal/acp round does:
// initialize -> session/new (with a fake quackmcp MCP server) -> session/prompt,
// asserting the update stream and that a bridged quackmcp_* call lands.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { createServer } from "node:http";
import { readFileSync, readdirSync, rmSync, statSync } from "node:fs";
import { strict as assert } from "node:assert";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";

const here = dirname(fileURLToPath(import.meta.url));
const env = { ...process.env };
if (!process.env.PI_ACP_REAL) env.PI_ACP_PI_CMD = join(here, "fake-pi.mjs");
if (!env.OPENCODE_CONFIG_CONTENT)
  env.OPENCODE_CONFIG_CONTENT = JSON.stringify({
    provider: { quack: { options: { baseURL: "http://127.0.0.1:1/v1", apiKey: "unused" }, models: { stub: { limit: { context: 65536, output: 32768 } } } } },
    skills: { paths: ["/opt/quack/skills"] },
  });

// Stub OTLP collector: records every span POSTed to /v1/traces.
const otlpSpans = [];
const otlpSrv = createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    for (const rs of JSON.parse(body).resourceSpans || [])
      for (const ss of rs.scopeSpans || []) otlpSpans.push(...ss.spans);
    res.writeHead(200, { "content-type": "application/json" });
    res.end("{}");
  });
});
await new Promise((r) => otlpSrv.listen(0, "127.0.0.1", r));
const TRACE_ID = "0af7651916cd43dd8448eb211c80319c";
const PARENT_ID = "b7ad6b7169203331";
env.OTEL_EXPORTER_OTLP_ENDPOINT = `http://127.0.0.1:${otlpSrv.address().port}`;
env.TRACEPARENT = `00-${TRACE_ID}-${PARENT_ID}-01`;

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

// quack's clientHandler side of permission asks: allow first, deny the rest.
const permAsks = [];
createInterface({ input: shim.stdout }).on("line", (l) => {
  const m = JSON.parse(l);
  if (m.method === "session/update") updates.push(m.params.update);
  else if (m.method === "session/request_permission") {
    permAsks.push(m.params);
    const optionId = permAsks.length === 1 ? "allow" : "reject";
    shim.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: m.id, result: { outcome: { outcome: "selected", optionId } } }) + "\n");
  } else if (m.id !== undefined) {
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
// piDir is deterministic (keyed by session id) so session/load can
// find it again from a fresh shim process - no need to scan tmpdir by mtime.
const piDir = join(tmpdir(), "pi-acp-" + sess.sessionId);

if (!process.env.ACP_CMD) {
  // the shim materialized skills + bridge into the pi config dir
  assert.ok(statSync(piDir).isDirectory(), "no pi config dir for this session id");
  const wantSkills = JSON.parse(env.OPENCODE_CONFIG_CONTENT).skills?.paths;
  if (wantSkills) {
    const settings = JSON.parse(readFileSync(join(piDir, "settings.json"), "utf8"));
    assert.deepEqual(settings.skills, wantSkills);
  }
  const bridge = JSON.parse(readFileSync(join(piDir, "extensions", "quackmcp.json"), "utf8"));
  assert.equal(bridge.prefix, "quackmcp");
  assert.equal(bridge.tools[0].name, "stage_review");
  readFileSync(join(piDir, "extensions", "quackmcp.ts")); // extension generated

  const models = JSON.parse(readFileSync(join(piDir, "models.json"), "utf8"));
  assert.equal(models.providers.quack.models[0].contextWindow, 65536, "contextWindow not plumbed from config's limit.context");
  assert.equal(models.providers.quack.models[0].maxTokens, 32768, "maxTokens not plumbed from config's limit.output");
}

const prompt = process.env.PI_ACP_REAL
  ? "Call the quackmcp_stage_review tool with verdict approve, then reply done."
  : "do the thing";
const promptPromise = call("session/prompt", {
  sessionId: sess.sessionId,
  prompt: [{ type: "text", text: prompt }],
});
if (!process.env.ACP_CMD && !process.env.PI_ACP_REAL) {
  // #998: a steer sent mid-prompt must reach the live round, not error or wait.
  shim.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "_quack/steer", params: { text: "focus on X" } }) + "\n");
}
const resp = await promptPromise;
assert.equal(resp.stopReason, "end_turn");
if (!process.env.ACP_CMD && !process.env.PI_ACP_REAL) {
  const steered = updates.find((u) => u.sessionUpdate === "agent_message_chunk" && u.content?.text?.includes("[steered: focus on X]"));
  assert.ok(steered, "mid-round steer never reached the live session");
}

const kinds = updates.map((u) => u.sessionUpdate);
assert.ok(kinds.includes("agent_message_chunk"), `no message chunk in ${kinds}`);
if (!process.env.ACP_CMD) {
  // the bridged tool call must have landed on the fake MCP server
  assert.equal(mcpCalls.length, 1, `expected 1 mcp call, got ${JSON.stringify(mcpCalls)}`);
  assert.equal(mcpCalls[0].name, "stage_review");
  const btc = updates.find((u) => u.sessionUpdate === "tool_call" && u.title.startsWith("quackmcp_"));
  assert.ok(btc, "no quackmcp_* tool_call update relayed to quack");
  // the relay must be able to recover the real tool name from _meta, not
  // just the kind "other" (#1278) - never bash/read/... and never "other".
  assert.equal(btc._meta?.quack_mcp_tool, "stage_review", "MCP identity missing from _meta");
  assert.notEqual(btc.kind, "other", "MCP tool call still falls through to kind \"other\"");
  assert.ok(kinds.includes("usage_update"), "usage_update missing - quack metrics would go dark");
}
if (process.env.PI_ACP_REAL && !process.env.ACP_CMD) {
  // Real end-to-end resume proof: round 2 on a FRESH shim process must carry
  // round 1's turn into the model's prefix (mock-openai.mjs's GET /requests).
  const base = new URL(JSON.parse(env.OPENCODE_CONFIG_CONTENT).provider.quack.options.baseURL);
  const before = await fetch(`${base.origin}/requests`).then((r) => r.json());
  const { shim: shim2, call: call2 } = connectShim(env);
  await call2("initialize", { protocolVersion: 1, clientCapabilities: {} });
  await call2("session/load", {
    sessionId: sess.sessionId, cwd: process.cwd(),
    mcpServers: [{ type: "sse", name: "quackmcp", url: mcpUrl, headers: [] }],
  });
  const resp2 = await call2("session/prompt", { sessionId: sess.sessionId, prompt: [{ type: "text", text: "What did you just do?" }] });
  assert.equal(resp2.stopReason, "end_turn");
  const after = await fetch(`${base.origin}/requests`).then((r) => r.json());
  assert.ok(after.length > before.length, "round 2 never reached the model");
  const prefix = JSON.stringify(after[after.length - 1]);
  assert.ok(prefix.includes("Call the quackmcp_stage_review tool"),
    "round 2's prefix is missing round 1's turn - session/load did not resume");
  shim2.stdin.end();
  await new Promise((r) => shim2.on("exit", r));

  // Same deletion proof as the fake-pi block below, against real pi.
  const { shim: shim5, call: call5 } = connectShim(env);
  await call5("initialize", { protocolVersion: 1, clientCapabilities: {} });
  const sess5 = await call5("session/new", { cwd: process.cwd(), mcpServers: [] });
  await call5("session/prompt", { sessionId: sess5.sessionId, prompt: [{ type: "text", text: "hi" }] });
  shim5.stdin.end();
  await new Promise((r) => shim5.on("exit", r));

  const sessionsDir5 = join(tmpdir(), "pi-acp-" + sess5.sessionId, "pi-sessions");
  for (const f of readdirSync(sessionsDir5)) rmSync(join(sessionsDir5, f));

  const { shim: shim6, call: call6 } = connectShim(env);
  await call6("initialize", { protocolVersion: 1, clientCapabilities: {} });
  await assert.rejects(
    call6("session/load", { sessionId: sess5.sessionId, cwd: process.cwd(), mcpServers: [] }),
    /no persisted session/,
    "real pi: session/load must fail once its session file is gone");
  shim6.stdin.end();
  await new Promise((r) => shim6.on("exit", r));
}
if (!process.env.PI_ACP_REAL && !process.env.ACP_CMD) {
  // permission bridge: git push blocked locally (no ask), .env reads asked -
  // one allowed, one denied.
  const end = (id) => updates.find((u) => u.sessionUpdate === "tool_call_update" && u.toolCallId === id);
  assert.equal(end("call_g1").status, "failed", "git push not blocked");
  assert.ok(!permAsks.some((p) => JSON.stringify(p).includes("git push")), "config-deny went through an ACP round-trip");
  assert.equal(permAsks.length, 2, `expected 2 permission asks, got ${permAsks.length}`);
  assert.equal(permAsks[0].toolCall.rawInput.path, "app/.env");
  assert.equal(end("call_g2").status, "completed", ".env read not allowed after judge approval");
  assert.equal(end("call_g3").status, "failed", "denied .env read not refused");
  assert.ok(kinds.includes("agent_thought_chunk"));
  const tc = updates.find((u) => u.sessionUpdate === "tool_call");
  assert.equal(tc.kind, "execute");
  assert.equal(tc.rawInput.command, "echo hi");
  const tu = updates.find((u) => u.sessionUpdate === "tool_call_update");
  assert.equal(tu.status, "completed");
  assert.equal(tu.rawOutput.output, "hi\n");
  const usageUpdate = updates.find((u) => u.sessionUpdate === "usage_update" && u.used === 99);
  assert.ok(usageUpdate, "no usage_update with used === 99");
  assert.equal(usageUpdate._meta?.quack_prompt_tokens, 10, "prompt tokens missing from usage_update _meta");
  assert.equal(usageUpdate._meta?.quack_cached_tokens, 6, "cached tokens missing from usage_update _meta");
  assert.equal(usageUpdate._meta?.quack_completion_tokens, 1, "completion tokens missing from usage_update _meta");
}
// OTLP assertions: flush is fire-and-forget on agent_settled, so wait for it.
if (!process.env.ACP_CMD) {
  for (let i = 0; i < 50 && otlpSpans.length === 0; i++) await new Promise((r) => setTimeout(r, 100));
  assert.equal(otlpSpans.length, 7, `expected the 7 parity spans, got ${otlpSpans.length}`);
  assert.ok(otlpSpans.every((sp) => sp.traceId === TRACE_ID), "span not under quack's trace id");
  assert.ok(otlpSpans.every((sp) => sp.parentSpanId === PARENT_ID), "span not parented under the round span");
  const gens = otlpSpans.filter((sp) => sp.name.startsWith("chat "));
  const av = (sp, k) => sp.attributes.find((a) => a.key === k)?.value;
  if (process.env.PI_ACP_REAL) {
    assert.ok(gens.length >= 1, "no generation span posted from real pi");
  } else {
    assert.equal(gens.length, 2, `generation count != LLM calls: ${gens.map((g) => g.name)}`);
    assert.equal(av(gens[1], "gen_ai.usage.input_tokens").intValue, "10");
    assert.equal(av(gens[1], "gen_ai.usage.output_tokens").intValue, "2");
    assert.ok(av(gens[0], "quack.thinking").stringValue.includes("hmm"));
    const toolSpans = otlpSpans.filter((sp) => sp.name.startsWith("execute_tool "));
    assert.ok(toolSpans.some((sp) => sp.name.includes("quackmcp_")), "no quackmcp tool span");
    const pushSpan = toolSpans.find((sp) => av(sp, "gen_ai.tool.call.arguments")?.stringValue.includes("git push"));
    assert.equal(pushSpan.status.code, 2, "blocked git push span not marked error");
    assert.ok(toolSpans.every((sp) => av(sp, "tool_call_id")?.stringValue), "tool span missing tool_call_id");
    assert.ok(toolSpans.every((sp) => av(sp, "gen_ai.tool.call.result")?.stringValue.length <= 8192 + "…[truncated]".length), "tool result attr exceeds the 8KB cap");
  }
}
console.log("ok -", updates.length, "updates,", mcpCalls.length, "mcp call(s),", otlpSpans.length, "otlp span(s)");

// connectShim: a second shim process, auto-allowing every permission ask -
// fake-pi.mjs's 3 guarded calls fire even with mcpServers: [], and hang without a reply.
function connectShim(spawnEnv) {
  const s = spawn(argv[0], argv.slice(1), { env: spawnEnv, stdio: ["pipe", "pipe", "inherit"] });
  const pend = new Map();
  let id = 1;
  createInterface({ input: s.stdout }).on("line", (l) => {
    if (!l.trim()) return;
    const m = JSON.parse(l);
    if (m.method === "session/request_permission") {
      s.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: m.id, result: { outcome: { outcome: "selected", optionId: "allow" } } }) + "\n");
    } else if (m.id !== undefined && pend.has(m.id)) {
      const { resolve, reject } = pend.get(m.id);
      pend.delete(m.id);
      m.error ? reject(new Error(m.error.message)) : resolve(m.result);
    }
  });
  const call2 = (method, params) => {
    const rid = id++;
    s.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: rid, method, params }) + "\n");
    return new Promise((resolve, reject) => pend.set(rid, { resolve, reject }));
  };
  return { shim: s, call: call2 };
}

// session/load on a FRESH shim process must resume, not start blank - proven
// via fake-pi.mjs's per-prompt turn counter surviving into a second process.
if (!process.env.ACP_CMD && !process.env.PI_ACP_REAL) {
  const turnsFile = join(piDir, "pi-sessions", sess.sessionId + ".turns");
  assert.equal(readFileSync(turnsFile, "utf8").trim().split("\n").length, 1, "round 1 did not record a turn");

  const { shim: shim2, call: call2 } = connectShim(env);
  const init2 = await call2("initialize", { protocolVersion: 1, clientCapabilities: {} });
  assert.equal(init2.agentCapabilities.loadSession, true, "shim must advertise loadSession for resume to fire");
  await call2("session/load", { sessionId: sess.sessionId, cwd: process.cwd(), mcpServers: [] });
  const resp2 = await call2("session/prompt", { sessionId: sess.sessionId, prompt: [{ type: "text", text: "again" }] });
  assert.equal(resp2.stopReason, "end_turn");
  assert.equal(readFileSync(turnsFile, "utf8").trim().split("\n").length, 2,
    "session/load spawned pi against a different session id/dir than session/new used - resume broken");
  shim2.stdin.end();
  // Fire-and-forget otel flush on stdin "end" - wait for real exit or it
  // races otlpSrv.close() below.
  await new Promise((r) => shim2.on("exit", r));
}

// A missing/deleted session file must fail session/load deterministically -
// never let pi silently start a blank session that quack records as resumed.
if (!process.env.ACP_CMD && !process.env.PI_ACP_REAL) {
  const { shim: shim3, call: call3 } = connectShim(env);
  await call3("initialize", { protocolVersion: 1, clientCapabilities: {} });
  const sess3 = await call3("session/new", { cwd: process.cwd(), mcpServers: [] });
  await call3("session/prompt", { sessionId: sess3.sessionId, prompt: [{ type: "text", text: "hi" }] });
  shim3.stdin.end();
  await new Promise((r) => shim3.on("exit", r));

  const sessionsDir = join(tmpdir(), "pi-acp-" + sess3.sessionId, "pi-sessions");
  for (const f of readdirSync(sessionsDir)) rmSync(join(sessionsDir, f));

  const { shim: shim4, call: call4 } = connectShim(env);
  await call4("initialize", { protocolVersion: 1, clientCapabilities: {} });
  await assert.rejects(
    call4("session/load", { sessionId: sess3.sessionId, cwd: process.cwd(), mcpServers: [] }),
    /no persisted session/,
    "session/load must fail, not silently start blank, once its session file is gone");
  shim4.stdin.end();
  await new Promise((r) => shim4.on("exit", r));
}

shim.stdin.end();
mcpSrv.close();
otlpSrv.close();

// no limit.context configured -> omit contextWindow rather than write 0, so
// pi keeps its own default instead of tripping provider-composer's reject.
if (!process.env.ACP_CMD && !process.env.PI_ACP_REAL) {
  const noLimitEnv = { ...env, OPENCODE_CONFIG_CONTENT: JSON.stringify({
    provider: { quack: { options: { baseURL: "http://127.0.0.1:1/v1", apiKey: "unused" }, models: { stub: {} } } },
  }) };
  const { shim: shim2, call: call2 } = connectShim(noLimitEnv);
  await call2("initialize", { protocolVersion: 1, clientCapabilities: {} });
  const sess2 = await call2("session/new", { cwd: process.cwd(), mcpServers: [] });
  const models2 = JSON.parse(readFileSync(join(tmpdir(), "pi-acp-" + sess2.sessionId, "models.json"), "utf8"));
  assert.ok(!("contextWindow" in models2.providers.quack.models[0]), "contextWindow written when unset");
  assert.ok(!("maxTokens" in models2.providers.quack.models[0]), "maxTokens written when unset");
  shim2.stdin.end();
}

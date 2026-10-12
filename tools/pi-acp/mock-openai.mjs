#!/usr/bin/env node
// Minimal OpenAI chat-completions stub. If the request offers a
// quackmcp_stage_review tool and no tool result yet, it calls it once -
// exercising the bridged tool path end to end with a real pi.
// A prompt naming a SCRIPTS key instead replays that key's tool calls, one per model request,
// so a real engine's gate, steer and cancel paths run without a live model.
import { createServer } from "node:http";
const port = process.env.PORT || 8091;

const SCRIPTS = {
  GATE: [
    ["bash", { command: "git push origin main" }],
    ["read", { path: "app/.env" }],
    ["read", { path: "app/.env.prod" }],
    ["bash", { command: "git clone --depth 1 https://example.invalid/x.git" }],
  ],
  STEER: [["bash", { command: "echo steer" }]],
  "Create a file named hello.txt": [["write", { path: "hello.txt", content: "hi" }]],
};
// STEER's first reply and every STALL reply stream this slowly, so a steer or cancel lands mid-prompt.
const SLOW_MS = { STEER: 1500, STALL: 8000 };

// Every request's message list, oldest first - GET /requests exposes it so
// a resume test can assert round 2's prefix actually carries round 1's turn.
const requests = [];
// Full request bodies minus messages: GET /bodies lets a test compare engines' wire shape (roles, max tokens, effort).
const bodies = [];

const textOf = (m) => (typeof m.content === "string" ? m.content : (m.content || []).map((c) => c.text || "").join(""));

// script finds the newest user turn naming a SCRIPTS/SLOW_MS key and how many tool results follow it.
function script(messages) {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role !== "user") continue;
    const key = Object.keys({ ...SCRIPTS, ...SLOW_MS }).find((k) => textOf(messages[i]).includes(k));
    if (key) return { key, step: messages.slice(i).filter((m) => m.role === "tool").length };
  }
  return null;
}

createServer((req, res) => {
  if (req.method === "GET" && req.url === "/requests") {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify(requests));
  }
  if (req.method === "GET" && req.url === "/bodies") {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify(bodies));
  }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", async () => {
    const rq = JSON.parse(body);
    requests.push(rq.messages || []);
    bodies.push({ ...rq, messages: undefined, roles: (rq.messages || []).map((m) => m.role), tools: (rq.tools || []).map((t) => t.function?.name) });
    const hasTool = (rq.tools || []).some((t) => t.function?.name === "quackmcp_stage_review");
    const answered = (rq.messages || []).some((m) => m.role === "tool");
    const s = script(rq.messages || []);
    const scripted = s && SCRIPTS[s.key]?.[s.step];
    const call = scripted
      ? { id: `tc_${s.key}_${s.step}`, type: "function", function: { name: scripted[0], arguments: JSON.stringify(scripted[1]) } }
      : !s && hasTool && !answered
        ? { id: "tc1", type: "function", function: { name: "quackmcp_stage_review", arguments: '{"verdict":"approve"}' } }
        : null;
    const slow = s && SLOW_MS[s.key] && s.step === 0 ? SLOW_MS[s.key] : 0;
    // cached_tokens makes the engine report cacheRead, so usage_update's prompt/cached split is exercised.
    const usage = { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12, prompt_tokens_details: { cached_tokens: 4 } };

    const sse = (d, fin = null, extra = {}) =>
      res.write("data: " + JSON.stringify({ id: "1", object: "chat.completion.chunk", model: "stub", choices: [{ index: 0, delta: d, finish_reason: fin }], ...extra }) + "\n\n");
    if (rq.stream) {
      res.writeHead(200, { "content-type": "text/event-stream" });
      sse({ role: "assistant" });
      sse({ reasoning_content: "pondering" });
      for (let t = 0; t < slow && !res.destroyed; t += 250) {
        sse({ content: "." });
        await new Promise((r) => setTimeout(r, 250));
      }
      if (res.destroyed) return;
      if (call) sse({ tool_calls: [{ index: 0, ...call }] });
      else sse({ content: "done" });
      sse({}, call ? "tool_calls" : "stop", { usage });
      res.end("data: [DONE]\n\n");
    } else {
      res.writeHead(200, { "content-type": "application/json" });
      const message = call ? { role: "assistant", content: null, tool_calls: [call] } : { role: "assistant", content: "done" };
      res.end(JSON.stringify({ id: "1", object: "chat.completion", model: "stub", choices: [{ index: 0, message, finish_reason: call ? "tool_calls" : "stop" }], usage }));
    }
  });
}).listen(port, function () { console.error("mock-openai on :" + this.address().port); });

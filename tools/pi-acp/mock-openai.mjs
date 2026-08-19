#!/usr/bin/env node
// Minimal OpenAI chat-completions stub. If the request offers a
// quackmcp_stage_review tool and no tool result yet, it calls it once -
// exercising the bridged tool path end to end with a real pi.
import { createServer } from "node:http";
const port = process.env.PORT || 8091;

createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const rq = JSON.parse(body);
    const hasTool = (rq.tools || []).some((t) => t.function?.name === "quackmcp_stage_review");
    const answered = (rq.messages || []).some((m) => m.role === "tool");
    const callTool = hasTool && !answered;

    const sse = (d, fin = null, extra = {}) =>
      res.write("data: " + JSON.stringify({ id: "1", object: "chat.completion.chunk", model: "stub", choices: [{ index: 0, delta: d, finish_reason: fin }], ...extra }) + "\n\n");
    if (rq.stream) {
      res.writeHead(200, { "content-type": "text/event-stream" });
      sse({ role: "assistant" });
      if (callTool)
        sse({ tool_calls: [{ index: 0, id: "tc1", type: "function", function: { name: "quackmcp_stage_review", arguments: '{"verdict":"approve"}' } }] });
      else sse({ content: "done" });
      sse({}, callTool ? "tool_calls" : "stop", { usage: { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 } });
      res.end("data: [DONE]\n\n");
    } else {
      res.writeHead(200, { "content-type": "application/json" });
      const message = callTool
        ? { role: "assistant", content: null, tool_calls: [{ id: "tc1", type: "function", function: { name: "quackmcp_stage_review", arguments: '{"verdict":"approve"}' } }] }
        : { role: "assistant", content: "done" };
      res.end(JSON.stringify({ id: "1", object: "chat.completion", model: "stub", choices: [{ index: 0, message, finish_reason: callTool ? "tool_calls" : "stop" }], usage: { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 } }));
    }
  });
}).listen(port, () => console.error("mock-openai on :" + port));

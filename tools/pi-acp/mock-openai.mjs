#!/usr/bin/env node
// Minimal OpenAI chat-completions stub: streams one canned assistant reply.
import { createServer } from "node:http";
const port = process.env.PORT || 8091;
createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const stream = body.includes('"stream":true') || body.includes('"stream": true');
    const msg = "pong";
    if (stream) {
      res.writeHead(200, { "content-type": "text/event-stream" });
      const chunk = (d) => res.write("data: " + JSON.stringify({ id: "1", object: "chat.completion.chunk", model: "stub", choices: [{ index: 0, delta: d, finish_reason: null }] }) + "\n\n");
      chunk({ role: "assistant" });
      chunk({ content: msg });
      res.write("data: " + JSON.stringify({ id: "1", object: "chat.completion.chunk", model: "stub", choices: [{ index: 0, delta: {}, finish_reason: "stop" }], usage: { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 } }) + "\n\n");
      res.end("data: [DONE]\n\n");
    } else {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ id: "1", object: "chat.completion", model: "stub", choices: [{ index: 0, message: { role: "assistant", content: msg }, finish_reason: "stop" }], usage: { prompt_tokens: 10, completion_tokens: 2, total_tokens: 12 } }));
    }
  });
}).listen(port, () => console.error("mock-openai on :" + port));

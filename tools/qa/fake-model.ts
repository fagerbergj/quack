import { createServer, type Server } from 'node:http';

export const FAKE_MODEL = 'qa-fake';

/** One canned answer: text, or a single tool call. */
export type FakeReply = { content: string } | { tool: { name: string; arguments: object } };
/** Picks the answer from the chat request body (its messages and offered tools). */
export type FakeScript = (request: { messages?: Array<{ role: string; content?: unknown }>; tools?: Array<{ function?: { name?: string } }> }) => FakeReply;

const parse = (body: string) => {
  try { return JSON.parse(body); } catch { return {}; }
};

function choice(reply: FakeReply, stream: boolean) {
  const call = 'tool' in reply && { index: 0, id: 'qa-call-1', type: 'function', function: { name: reply.tool.name, arguments: JSON.stringify(reply.tool.arguments) } };
  const finish = call ? 'tool_calls' : 'stop';
  const content = 'content' in reply ? reply.content : '';
  if (!stream) return { index: 0, message: { role: 'assistant', content, ...(call && { tool_calls: [call] }) }, finish_reason: finish };
  return { index: 0, delta: { role: 'assistant', content, ...(call && { tool_calls: [call] }) }, finish };
}

/** An OpenAI-compatible endpoint that answers every chat request with `reply` (or what `reply` picks), so seeded sessions need no real model. */
export function fakeModel(reply: string | FakeScript): Promise<{ server: Server; url: string }> {
  const script: FakeScript = typeof reply === 'string' ? () => ({ content: reply }) : reply;
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', (c) => (body += c));
    req.on('end', () => {
      const request = parse(body);
      const stream = request.stream === true;
      const base = { id: 'qa', object: stream ? 'chat.completion.chunk' : 'chat.completion', created: 0, model: FAKE_MODEL };
      const usage = { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 };
      const picked = choice(script(request), stream);
      if (!stream) return res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ ...base, choices: [picked], usage }));
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      const send = (c: object, extra = {}) => res.write(`data: ${JSON.stringify({ ...base, choices: [c], ...extra })}\n\n`);
      const { finish, ...delta } = picked as { finish: string; index: number; delta: object };
      send(delta);
      send({ index: 0, delta: {}, finish_reason: finish }, { usage });
      res.end('data: [DONE]\n\n');
    });
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ server, url: `http://127.0.0.1:${(server.address() as { port: number }).port}/v1` })));
}

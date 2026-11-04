import { createServer, type Server } from 'node:http';

export const FAKE_MODEL = 'qa-fake';

const streams = (body: string) => {
  try { return JSON.parse(body).stream === true; } catch { return false; }
};

/** An OpenAI-compatible endpoint that answers every chat request with `reply`, so seeded sessions need no real model. */
export function fakeModel(reply: string): Promise<{ server: Server; url: string }> {
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', (c) => (body += c));
    req.on('end', () => {
      const stream = streams(body);
      const base = { id: 'qa', object: stream ? 'chat.completion.chunk' : 'chat.completion', created: 0, model: FAKE_MODEL };
      const usage = { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 };
      if (!stream) return res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ ...base, choices: [{ index: 0, message: { role: 'assistant', content: reply }, finish_reason: 'stop' }], usage }));
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      const send = (choice: object, extra = {}) => res.write(`data: ${JSON.stringify({ ...base, choices: [choice], ...extra })}\n\n`);
      send({ index: 0, delta: { role: 'assistant', content: '' } });
      send({ index: 0, delta: { content: reply } });
      send({ index: 0, delta: {}, finish_reason: 'stop' }, { usage });
      res.end('data: [DONE]\n\n');
    });
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ server, url: `http://127.0.0.1:${(server.address() as { port: number }).port}/v1` })));
}

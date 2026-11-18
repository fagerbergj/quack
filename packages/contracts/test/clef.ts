import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import type { TestContext } from 'node:test';
import { createProvider, envApiKeyAuth } from '@earendil-works/pi-ai';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';

type Sent = { type: string; instructions?: string; criteria: Record<string, string> };
/** One decision-model request as Clef received it: its state, and each question's instructions by id. */
export type ClefRequest = { state: unknown; questions: Record<string, string> };

type Packed = { asks?: string[]; items?: Array<{ id: string }> };
// A packed question as the model reads it: its item named by the item's id and its text taken from `asks` when it points there.
function readAs(state: Packed, instructions: string): string {
  const m = /^About items\[(\d+)\]: (.*)$/s.exec(instructions);
  if (!m || !state.items) return instructions;
  const ask = /^answer asks\[(\d+)\]\.$/.exec(m[2]);
  return `About items[${m[1]}] (${state.items[Number(m[1])].id}): ${ask ? state.asks![Number(ask[1])] : m[2]}`;
}

/** A stand-in for Clef on its wire format. A choice gets `pick(instructions)` (the first option by name when it gives none) at 0.95;
 * a bool gets an unknown answer type, so it escalates. */
export async function fakeClef(t: TestContext, pick: (instructions: string) => string | undefined = () => undefined) {
  const asked: string[][] = [];
  const requests: ClefRequest[] = [];
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const { state, questions: raw } = JSON.parse(body) as { state: Packed; questions: Record<string, Sent> };
    const questions = Object.keys(raw);
    const sent = Object.fromEntries(questions.map((q) => [q, { ...raw[q], instructions: readAs(state, raw[q].instructions ?? '') }]));
    asked.push(questions);
    requests.push({ state, questions: Object.fromEntries(questions.map((q) => [q, sent[q].instructions])) });
    const choice = (q: Sent) => {
      const options = Object.keys(q.criteria).sort();
      const first = pick(q.instructions ?? '') ?? options[0];
      const rest = options.filter((o) => o !== first);
      return { type: 'choice', choice: first, confidence: 0.95, probabilities: Object.fromEntries([[first, 0.95], ...rest.map((k) => [k, 0.05 / rest.length])]) };
    };
    const answers = Object.fromEntries(questions.map((q) => [q, sent[q].type === 'choice' ? choice(sent[q]) : { type: 'noul', noul: 0.95 }]));
    res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ answers, usage: { input_tokens: 1, output_tokens: 0 } }));
  }).listen(0);
  await new Promise((r) => server.once('listening', r));
  const key = process.env.QUACK_CLEF_KEY;
  process.env.QUACK_CLEF_KEY = 'none';
  t.after(() => (server.close(), key === undefined ? delete process.env.QUACK_CLEF_KEY : (process.env.QUACK_CLEF_KEY = key)));
  const baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`;
  const clef = { id: 'clef', name: 'clef', type: 'classifier', api: 'typesafe-system-one', provider: 'clef', baseUrl, input: ['text'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 8192 };
  return { asked, requests, provider: createProvider({ id: 'clef', baseUrl, auth: { apiKey: envApiKeyAuth('clef', ['QUACK_CLEF_KEY']) }, models: [clef] as never, classifiers: { 'typesafe-system-one': typesafeSystemOneApi() } }) };
}

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT, withAbortSignal } from '@earendil-works/chord/context';
import { createModels, createProvider } from '@earendil-works/pi-ai';
import { openAICompletionsApi } from '@earendil-works/pi-ai/api/openai-completions.lazy';
import { decide, MODEL_RETRY, modelResolver, type DecisionPoint } from '../src/index.ts';

const point: DecisionPoint = { name: 'c', question: 'Does it hold?', options: ['yes', 'no'] };
const model = { id: '27b', name: '27b', api: 'openai-completions', provider: 'local', baseUrl: 'http://llm.test/v1', reasoning: false, input: ['text'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 8192, maxTokens: 512 };
const auth = { apiKey: { name: 'key', resolve: async () => ({ auth: { apiKey: 'k' }, source: 'test' }) } };

// Real pi-ai openai-completions provider over a scripted transport, so a status and Retry-After take the path a llama-swap reply does.
const chunk = (delta: object, finish: string | null) => `data: ${JSON.stringify({ id: 'x', object: 'chat.completion.chunk', created: 0, model: '27b', choices: [{ index: 0, delta, finish_reason: finish }] })}\n\n`;
const answer = () => new Response(chunk({ role: 'assistant', content: '{"choice": "yes", "confidence": 0.9, "reason": "holds"}' }, null) + chunk({}, 'stop') + 'data: [DONE]\n\n', { headers: { 'content-type': 'text/event-stream' } });
const BUSY = { message: 'Too many requests', type: 'rate_limit_error', code: 'concurrency_limit' };
const refused = (status: number, headers: Record<string, string> = {}, body: object = BUSY) => () => new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json', ...headers } });

function rig(replies: Array<() => Response>, sleep?: (ms: number, signal?: AbortSignal) => Promise<void>) {
  const models = createModels();
  models.setProvider(createProvider({ id: 'local', baseUrl: model.baseUrl, auth: auth as never, models: [model] as never, api: openAICompletionsApi() }));
  const sent: number[] = [];
  const slept: number[] = [];
  const logs: string[] = [];
  const fetch = (async () => (sent.push(sent.length + 1), replies.shift()!())) as typeof globalThis.fetch;
  const seams = { fetch, random: () => 0, log: (l: string) => logs.push(l), sleep: sleep ?? (async (ms: number) => void slept.push(ms)) };
  const memo = new Map<string, unknown>();
  const api = { models, now: () => 0, memo: async (k: string, ...rest: unknown[]) => (rest.length === 2 && !memo.has(k) && memo.set(k, rest[0]), memo.get(k)) };
  return { resolver: modelResolver('local', '27b', seams), api: api as never, sent, slept, logs };
}

test('a 429 then an answer decides on the second attempt, after the Retry-After delay', async () => {
  const r = rig([refused(429, { 'retry-after': '5' }), answer]);
  const d = await r.resolver.resolve(point, r.api, BACKGROUND_CONTEXT);
  assert.equal(d?.choice, 'yes');
  assert.deepEqual([r.sent.length, r.slept], [2, [5000]], 'Retry-After 5 s is over the 2 s base, so it sets the wait');
  assert.equal(r.logs.length, 1);
  assert.match(r.logs[0], /session=\S+ model=local\/27b status=429 attempt=1\/9/);
});

test('three 503s then an answer decides, backing off exponentially', async () => {
  const r = rig([refused(503), refused(503), refused(503), answer]);
  assert.equal((await r.resolver.resolve(point, r.api, BACKGROUND_CONTEXT))?.choice, 'yes');
  assert.deepEqual(r.slept, [2000, 4000, 8000]);
});

test('a 400 is not retried', async () => {
  const r = rig([refused(400, {}, { message: 'messages: field required', type: 'invalid_request_error' }), answer]);
  assert.equal(await r.resolver.resolve(point, r.api, BACKGROUND_CONTEXT), undefined);
  assert.deepEqual([r.sent.length, r.logs], [1, []]);
});

test('an input the model says is too large is not retried, though the host answers 500', async () => {
  const over = { error: { code: 500, message: 'input (8288 tokens) is too large to process. increase the physical batch size (current batch size: 8192)', type: 'server_error' } };
  const r = rig([refused(500, {}, over), answer]);
  assert.equal(await r.resolver.resolve(point, r.api, BACKGROUND_CONTEXT), undefined);
  assert.deepEqual([r.sent.length, r.logs], [1, []]);
});

test('exhausted retries leave the decision undecided', async () => {
  const r = rig(Array.from({ length: MODEL_RETRY.maxRetries + 2 }, () => refused(503)));
  assert.equal(await decide(r.api, point, [r.resolver], BACKGROUND_CONTEXT), undefined);
  assert.equal(r.sent.length, MODEL_RETRY.maxRetries + 1, 'bounded: the first call and nine retries');
  assert.ok(r.slept.every((ms) => ms <= MODEL_RETRY.maxAgentDelayMs));
});

test('an abort during the backoff stops at once, with no further call', async () => {
  const ac = new AbortController();
  const waiting = (_ms: number, signal?: AbortSignal) => new Promise<void>((_, reject) => {
    signal!.addEventListener('abort', () => reject(signal!.reason), { once: true });
    ac.abort(new Error('closing'));
  });
  const r = rig([refused(503), answer], waiting);
  await assert.rejects(r.resolver.resolve(point, r.api, withAbortSignal(ac.signal, BACKGROUND_CONTEXT)), /closing/);
  assert.equal(r.sent.length, 1);
});

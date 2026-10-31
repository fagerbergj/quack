import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { decide, parseDecision, sessionModel, type DecisionPoint, type Resolver } from '../src/index.ts';

const point: DecisionPoint = { name: 'flex', question: 'Who starts at flex?', options: ['Pollard', 'Stevenson'], minConfidence: 0.6 };

test('parses a JSON answer, even with prose around it', () => {
  assert.deepEqual(parseDecision('Sure. {"choice": "Pollard", "confidence": 0.8, "reason": "indoors"} Done.', point), {
    choice: 'Pollard',
    confidence: 0.8,
    reason: 'indoors',
  });
});

test('rejects answers outside the options or without a valid confidence', () => {
  assert.equal(parseDecision('{"choice": "Walker", "confidence": 0.9}', point), undefined);
  assert.equal(parseDecision('{"choice": "Pollard"}', point), undefined);
  assert.equal(parseDecision('{"choice": "Pollard", "confidence": 3}', point), undefined);
  assert.equal(parseDecision('no json here', point), undefined);
});

function fakeApi() {
  const memo = new Map<string, unknown>();
  return {
    // Mirrors ToolExecutionApi.memo: (name, ctx) reads; (name, candidate, ctx) stores the first candidate.
    memo: async (name: string, ...rest: unknown[]) => {
      if (rest.length === 2 && !memo.has(name)) memo.set(name, rest[0]);
      return memo.get(name);
    },
  };
}

const answering = (id: string, choice: string, confidence: number, calls: string[]): Resolver => ({
  id,
  resolve: async () => {
    calls.push(id);
    return { choice, confidence, reason: id };
  },
});

test('escalates past a low-confidence or missing answer, then memoizes the decision', async () => {
  const calls: string[] = [];
  const skip: Resolver = { id: 'skip', resolve: async () => { calls.push('skip'); return undefined; } };
  const chain = [skip, answering('session', 'Stevenson', 0.4, calls), answering('clef', 'Pollard', 0.9, calls)];
  const api = fakeApi() as never;

  const first = await decide(api, point, chain, BACKGROUND_CONTEXT);
  assert.deepEqual(first, { choice: 'Pollard', confidence: 0.9, reason: 'clef', resolver: 'clef' });
  assert.deepEqual(calls, ['skip', 'session', 'clef']);

  const replay = await decide(api, point, chain, BACKGROUND_CONTEXT);
  assert.deepEqual(replay, first);
  assert.deepEqual(calls, ['skip', 'session', 'clef'], 'a replay asks no resolver again');
});

test('returns undefined when every resolver escalates', async () => {
  const calls: string[] = [];
  assert.equal(await decide(fakeApi() as never, point, [answering('session', 'Pollard', 0.2, calls)], BACKGROUND_CONTEXT), undefined);
});

test('the session resolver escalates when nothing the model said can be forked, paging past long tool rounds', async () => {
  // 100 tool entries after the newest assistant entry spill onto a second page of the descending scan.
  const entries = [...Array.from({ length: 100 }, (_, i) => ({ id: 200 - i, kind: 'pi.tool' })), { id: 100, kind: 'pi.assistant' }, { id: 99, kind: 'pi.user' }];
  let forkedAt: unknown;
  const tx = (items: typeof entries) => ({
    scanEntries: async (_q: unknown, limit: number, cursor?: number) => {
      const from = cursor ?? 0;
      return { items: items.slice(from, from + limit), next: from + limit < items.length ? from + limit : undefined };
    },
    forkConversation: async (_c: unknown, at: unknown) => { forkedAt = at; throw new Error('stop after the fork point is found'); },
  });
  const api = (items: typeof entries) => ({ ...fakeApi(), conversationId: 1, taskId: 1, commit: (fn: (t: unknown) => unknown) => fn(tx(items)) }) as never;

  assert.equal(await sessionModel.resolve(point, api([{ id: 1, kind: 'pi.user' }]), BACKGROUND_CONTEXT), undefined, 'no assistant entry yet');
  await assert.rejects(sessionModel.resolve(point, api(entries), BACKGROUND_CONTEXT), /stop after/);
  assert.equal(forkedAt, 99, 'the entry just before the newest assistant message, found on the second page');
});

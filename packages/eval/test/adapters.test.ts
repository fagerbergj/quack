import assert from 'node:assert/strict';
import { test } from 'node:test';
import type { EvalAdapter, ExtensionManifest } from '@quack/sdk';
import { adapterScores, adaptersOf, blocksOf, servicesOf, type SessionRecord } from '../src/index.ts';

const stub = async () => ({ env: {}, captured: [], close: async () => {} });
const note: EvalAdapter = {
  kind: 'acme.note',
  services: { 'acme.world': stub },
  blocks: (c) => c === 'no',
  judge: {
    rubric: { name: 'acme', version: 1, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'long', weight: 'gate', code: (a: string) => ({ met: a.length > 2 ? 1 : 0 }) }] }] },
    evidence: async (_s, content) => (content === 'skip' ? undefined : {}),
  },
};
const adapters = adaptersOf([{ name: 'acme', evalAdapters: [note] }, { name: 'bare' }] as ExtensionManifest[]);
const record = (content: string): SessionRecord => ({ inputs: [], outcome: { status: 'pending' }, turns: 0, usage: { input: 0, output: 0, cacheRead: 0 }, docs: {}, artifacts: { 'acme.note': [{ kind: 'acme.note', version: 1, hash: 'h', content, sealedAt: 0, verdict: null }] } });

test('adapters are looked up by kind from the manifests that carry them, so eval names no extension', () => {
  assert.deepEqual(adapters, [note]);
  assert.deepEqual(Object.keys(servicesOf(adapters)), ['acme.world']);
  assert.equal(blocksOf(adapters)({ 'acme.note': 'no' }), true);
  assert.equal(blocksOf(adapters)({ 'acme.note': 'yes' }), false);
  assert.equal(blocksOf(adapters)({ 'other.kind': 'no' }), undefined);
});

test('an adapter scores a recording with its rubric code checks, and skips an artifact its evidence declines', async () => {
  assert.deepEqual(await adapterScores(adapters, record('long enough')), { 'acme.note.code_score': 1, 'acme.note.code_gates_failed': 0 });
  assert.deepEqual(await adapterScores(adapters, record('ab')), { 'acme.note.code_score': 1, 'acme.note.code_gates_failed': 1 });
  assert.deepEqual(await adapterScores(adapters, record('skip')), {});
});

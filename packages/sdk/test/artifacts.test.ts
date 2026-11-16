import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { LiveDoc, type ToolExecutionApi, type ToolRegistration } from '@earendil-works/pi-durable';
import { Artifacts, artifactsExtension, type ArtifactKind, type SealedArtifact } from '../src/index.ts';

const note: ArtifactKind = { kind: 'test.note', path: 'artifacts/note.md', tool: 'submit_note' };
const delivered = (): SealedArtifact => ({ kind: note.kind, version: 1, hash: 'h1', content: 'ok', sealedAt: 0, verdict: { passed: true }, final: { input: 'in-1', refused: 0 } });

// One call of the seal tool as its task sees the session: a changed file, the run's one input, the artifacts doc, and the call's memos, which a replay of the call shares.
function callApi(doc: { kinds: Record<string, { versions: SealedArtifact[] }> }, memo = new Map<string, unknown>()) {
  const api = {
    conversationId: 'c',
    now: () => 0,
    env: { readTextFile: async () => ({ ok: true, value: 'ok, and better' }) },
    snapshot: async (token: unknown) => (token === LiveDoc ? { run: { inputs: ['in-1'] } } : token === Artifacts ? structuredClone(doc) : undefined),
    commit: async (change: (tx: unknown) => unknown) => change({ doc: async () => doc }),
    memo: async (name: string, ...rest: unknown[]) => {
      if (rest.length === 2 && !memo.has(name)) memo.set(name, rest[0]);
      return memo.get(name);
    },
  };
  return { api: api as unknown as ToolExecutionApi, memo };
}

const sealTool = (artifactsExtension([note]) as unknown as { tools: ToolRegistration[] }).tools[0];
const outcome = (r: { isError?: boolean; control?: { terminate?: boolean } }) => (r.control?.terminate ? 'deny' : r.isError ? 'refused' : 'sealed');

test('a seal call replayed after a crash counts its refusal once; only a later call ends the run', async () => {
  const doc = { kinds: { [note.kind]: { versions: [delivered()] } } };
  const first = callApi(doc);
  assert.equal(outcome(await sealTool.execute({}, first.api, BACKGROUND_CONTEXT)), 'refused');
  assert.equal(doc.kinds[note.kind].versions[0].final!.refused, 1);
  // The runtime reruns a replay-safe call whose result was not committed, with that call's memos.
  assert.equal(outcome(await sealTool.execute({}, callApi(doc, first.memo).api, BACKGROUND_CONTEXT)), 'refused', 'the replay is still the first refusal');
  assert.equal(doc.kinds[note.kind].versions[0].final!.refused, 1);
  assert.equal(outcome(await sealTool.execute({}, callApi(doc).api, BACKGROUND_CONTEXT)), 'deny', 'a second call is the second refusal');
  assert.deepEqual(doc.kinds[note.kind].versions.map((v) => v.version), [1], 'nothing past the delivered version is sealed');
});

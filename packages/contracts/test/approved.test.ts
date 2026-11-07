import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { Approvals, Artifacts, type Approval, type SealedArtifact } from '@quack/sdk';
import { approvedSpec } from '../src/approval.ts';
import { specEvidence, type ContractSpec } from '../src/index.ts';
import { scheduleSpecV1, scheduleSpecV2, SLEEPER_PAGE } from './fixtures.ts';

const refs = { versioning: 'request', time: 'request' };
const sealed = (version: number, spec: ContractSpec, passed: boolean | null, sealedAt: number): SealedArtifact =>
  ({ kind: 'contract.spec', version, hash: `h${version}`, content: JSON.stringify(spec), sealedAt, verdict: passed === null ? null : { passed } as never, outcome: '', summary: '' });
const record = (version: number, approval: string, sealedAt: number): SealedArtifact =>
  ({ kind: 'contract.approved', version: 1, hash: 'r', content: JSON.stringify({ version, note: '', approval }), sealedAt, verdict: null, outcome: `v${version}`, summary: '' });
const asked = (requestedAt: number, decision: 'approve' | 'reject'): Approval => ({ tool: 'propose_contract', args: {}, requestedAt, verdict: { decision, by: 'jf', reason: null, at: requestedAt + 1 }, executing: false, done: true });

// Spec versions: 1 fails, 1 passes and is approved, 2 passes and is rejected; the approved one is artifact 2.
const v1Draft = { ...scheduleSpecV1(refs), title: 'draft' };
const v2 = scheduleSpecV2(refs, []);
const history = {
  specs: [sealed(1, v1Draft, false, 10), sealed(2, scheduleSpecV1(refs), true, 20), sealed(3, v2, true, 40)],
  approvals: { a1: asked(25, 'approve'), a2: asked(45, 'reject') },
  approved: [record(1, 'a1', 30)],
};
const kindsOf = (specs: SealedArtifact[]) => ({ 'contract.spec': { versions: specs }, 'contract.approved': { versions: history.approved } });

test('the approved spec is the artifact the person approved, not the first draft that shares its version', () => {
  const found = approvedSpec(kindsOf(history.specs), history.approvals);
  assert.equal(found?.version, 2);
  assert.equal(approvedSpec(kindsOf(history.specs), history.approvals, 2), undefined, 'nothing was approved before artifact 2');
  assert.equal(approvedSpec(kindsOf(history.specs), {})?.version, 2, 'without its approval the newest passing match stands');
});

function fakeApi(specs: SealedArtifact[], approved = history.approved) {
  const docs = new Map<unknown, unknown>([[Artifacts, { kinds: { 'contract.spec': { versions: specs }, 'contract.approved': { versions: approved } } }], [Approvals, { items: history.approvals }]]);
  return { conversationId: 'c', env: undefined, snapshot: async (def: unknown) => docs.get(def) ?? { items: {} } } as never;
}
const breaksOf = async (api: never, judged: SealedArtifact) => ((await specEvidence()(api, judged, ctx)).facts[0].state as { breaking: unknown[] }).breaking;

test("a break a rejected version introduced is named again when the next version keeps it", async () => {
  const v3 = sealed(4, { ...v2, version: 3 }, null, 50);
  const breaking = await breaksOf(fakeApi([...history.specs, v3]), v3);
  assert.ok(breaking.length > 0, 'v3 is diffed against approved v1, not rejected v2');
});

// Before any approval the first passing version is what callers were first given, so breaks accumulate against it.
test('with nothing approved, a break is diffed against the first passing version, not the last', async () => {
  const draft = sealed(3, { ...v2, version: 3 }, null, 50);
  const specs = [sealed(1, scheduleSpecV1(refs), true, 20), sealed(2, scheduleSpecV2(refs, [SLEEPER_PAGE]), true, 40), draft];
  assert.ok((await breaksOf(fakeApi(specs, []), draft)).length > 0);
});

test('a record that names its artifact is exact, even when a later passing draft shares its spec version', () => {
  const exact: SealedArtifact = { ...record(1, 'a1', 30), content: JSON.stringify({ version: 1, artifact: 2, hash: 'h2', note: '', approval: 'a1' }) };
  const specs = [...history.specs.slice(0, 2), sealed(3, { ...scheduleSpecV1(refs), title: 'later draft' }, true, 22)];
  const all = { 'contract.spec': { versions: specs }, 'contract.approved': { versions: [exact] } };
  assert.equal(approvedSpec(all, history.approvals)?.version, 2);
  assert.equal(approvedSpec(all, history.approvals, 2), undefined);
});

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { Approvals, Artifacts, type Approval, type SealedArtifact } from '@quack/sdk';
import { approvedSpec, supersedeProposals } from '../src/approval.ts';
import { recordedSpecInputs, specInputs, type ContractSpec } from '../src/index.ts';
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
const breaksOf = async (api: never, judged: SealedArtifact) => (JSON.parse((await specInputs()(api, judged, ctx)).facts) as { breaking: unknown[] }).breaking;

test("a break a rejected version introduced is named again when the next version keeps it", async () => {
  const v3 = sealed(4, { ...v2, version: 3 }, null, 50);
  const breaking = await breaksOf(fakeApi([...history.specs, v3]), v3);
  assert.ok(breaking.length > 0, 'v3 is diffed against approved v1, not rejected v2');
});

// A passed draft was never given to callers, so with nothing approved there is no spec to diff against.
test('with nothing approved, no break is diffed against an earlier passing version', async () => {
  const draft = sealed(3, { ...v2, version: 3 }, null, 50);
  const specs = [sealed(1, scheduleSpecV1(refs), true, 20), sealed(2, scheduleSpecV2(refs, [SLEEPER_PAGE]), true, 40), draft];
  assert.deepEqual(await breaksOf(fakeApi(specs, []), draft), []);
});

test('a record that names its artifact is exact, even when a later passing draft shares its spec version', () => {
  const exact: SealedArtifact = { ...record(1, 'a1', 30), content: JSON.stringify({ version: 1, artifact: 2, hash: 'h2', note: '', approval: 'a1' }) };
  const specs = [...history.specs.slice(0, 2), sealed(3, { ...scheduleSpecV1(refs), title: 'later draft' }, true, 22)];
  const all = { 'contract.spec': { versions: specs }, 'contract.approved': { versions: [exact] } };
  assert.equal(approvedSpec(all, history.approvals)?.version, 2);
  assert.equal(approvedSpec(all, history.approvals, 2), undefined);
});

// The person approves v2 (artifact 3) and its record is written later; a judge run between the two must already diff against v2.
test('an approval decided but not yet recorded is already the base', async () => {
  const approved = sealed(3, v2, true, 40);
  const next = sealed(4, { ...v2, version: 3 }, null, 50);
  const approvals = { ...history.approvals, a3: { ...asked(41, 'approve'), args: { version: 2, artifact: 3, hash: 'h3' } } };
  const docs = new Map<unknown, unknown>([[Artifacts, { kinds: { 'contract.spec': { versions: [...history.specs.slice(0, 2), approved, next] }, 'contract.approved': { versions: history.approved } } }], [Approvals, { items: approvals }]]);
  const api = { conversationId: 'c', env: undefined, snapshot: async (def: unknown) => docs.get(def) ?? { items: {} } } as never;
  assert.deepEqual(await breaksOf(api, next), []);
});

// A replay shows what stood when the spec was sealed: an approval decided later is not its base.
test('a replay uses an unrecorded approval only if it was decided before the spec was sealed', async () => {
  const next = sealed(4, { ...v2, version: 3 }, null, 50);
  const breaksAt = async (decidedAt: number) => {
    const approvals = { ...history.approvals, a3: { ...asked(decidedAt - 1, 'approve'), args: { version: 2, artifact: 3, hash: 'h3' }, verdict: { decision: 'approve', by: 'jf', reason: null, at: decidedAt } } };
    const artifacts = { 'contract.spec': [...history.specs.slice(0, 3), next], 'contract.approved': history.approved };
    const inputs = await recordedSpecInputs({ docs: { 'quack.approvals': { items: approvals } }, artifacts, readFile: async () => undefined } as never, next.content);
    return (JSON.parse(inputs.facts) as { breaking: unknown[] }).breaking;
  };
  assert.deepEqual(await breaksAt(45), []);
  assert.ok((await breaksAt(60)).length > 0, 'decided after the seal, so v1 is still the base');
});

test('a verdict on a withdrawn proposal is never the base, while a record written before its withdrawal stands', () => {
  const withdrawn = { at: 50, reason: 'task_ended', supersededBy: null } as const;
  const late = { ...asked(41, 'approve'), args: { version: 2, artifact: 3, hash: 'h3' }, withdrawn };
  assert.equal(approvedSpec(kindsOf(history.specs), { ...history.approvals, a3: late })?.version, 2, 'artifact 3 was never approved; artifact 2 stays the base');
  assert.equal(approvedSpec(kindsOf(history.specs), { ...history.approvals, a1: { ...history.approvals.a1, withdrawn } })?.version, 2, 'a1 was recorded first');
});

test('a newer seal withdraws the open proposals of older artifacts only', async () => {
  const proposal = (artifact: number | undefined, more: Partial<Approval> = {}): Approval => ({ tool: 'propose_contract', args: artifact ? { version: 1, artifact } : { version: 1 }, requestedAt: 1, verdict: null, executing: false, done: false, ...more });
  const items: Record<string, Approval> = { older: proposal(1), same: proposal(2), legacy: proposal(undefined), ran: proposal(1, { verdict: asked(1, 'approve').verdict, executing: true }), other: { ...proposal(1), tool: 'start' } };
  const before = structuredClone(items);
  const api = { conversationId: 'c', now: () => 7, commit: async (fn: (tx: unknown) => Promise<unknown>) => fn({ doc: async () => ({ items }) }) } as never;
  await supersedeProposals(2, api, ctx);
  assert.deepEqual(items.older!.withdrawn, { at: 7, reason: 'superseded', supersededBy: 'contract.spec:2' });
  assert.deepEqual({ ...items, older: before.older }, before, 'the same artifact, a row without one, an executed one and another tool are left');
});

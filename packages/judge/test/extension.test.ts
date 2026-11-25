import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { defineExtension, defineTool, type Extension, type ToolRegistration } from '@earendil-works/pi-durable';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { obj } from '@earendil-works/pi-schema';
import { Approvals, Artifacts, artifactsExtension, gated, resolveApproval, type Approval, type ArtifactKind } from '@quack/sdk';
import { openSession, readExport } from '@quack/session';
import { subagentExtension } from '@quack/subagent';
import { judged, manifest, type JudgeConfig, type Judged } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-judge-ext-'));
after(() => rm(dir, { recursive: true, force: true }));

// What the note is written against, as a review's head commit; undefined leaves the note without a basis.
let head: string | undefined;
const note: ArtifactKind = { kind: 'test.note', path: 'artifacts/note.md', tool: 'submit_note', basis: async () => head };
const sayOk = { name: 'says-ok', version: 1, threshold: 1, criteria: [{ id: 'tone', checks: [{ id: 'says_ok', label: 'Does not say ok', weight: 'gate' as const, code: (a: string) => ({ met: a.includes('ok') ? 1 : 0, detail: 'the text must say ok' }) }] }] };
const how = (refuse?: Judged['refuse']): Judged => ({ rubric: sayOk, declaration: { scopes: {}, checks: {} }, inputs: async () => ({}), refuse });

const write = (content: string) => fauxAssistantMessage(fauxToolCall('write', { path: note.path, content }), { stopReason: 'toolUse' });
const submit = fauxAssistantMessage(fauxToolCall('submit_note', {}), { stopReason: 'toolUse' });
const done = fauxAssistantMessage('Done.');

type Reply = ReturnType<typeof fauxAssistantMessage>;
type Session = Awaited<ReturnType<typeof openSession>>;
// A step may act on the session before it answers, as a person steering mid-run does.
type Step = Reply | ((s: Session) => Promise<Reply>);

// One run of an agent that enables the judge with `config` and registers test.note, the way a feature's session setup does.
async function run(name: string, config: JudgeConfig, script: Step[], refuse?: Judged['refuse'], inputs = ['Write and submit the note.'], judging = how(refuse), extra: Extension[] = []) {
  const seen: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  let session: Session;
  faux.setResponses(script.map((step) => async (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), typeof step === 'function' ? step(session) : step)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const judge = await manifest.session!(config, {}, []);
  judged(note.kind, judging);
  session = await openSession({ dir: join(dir, name), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, ...judge, artifactsExtension([note]), ...extra] } });
  let result!: Awaited<ReturnType<Session['submit']>>;
  for (const [i, text] of inputs.entries()) result = await session.submit(text, `${name}-${i + 1}`, true);
  const versions = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))?.kinds[note.kind]?.versions ?? [];
  await session.close();
  const transcript = JSON.stringify((await readExport(join(dir, name))).transcript);
  return { seen, versions, transcript, asked: seen.length, answer: result.status === 'done' ? result.answer : undefined };
}

const passed = (v: { verdict: unknown }) => (v.verdict as { passed?: boolean } | null)?.passed ?? null;

test('a failed version comes back to revise; the passing one seals with the judge report as its note', async () => {
  const r = await run('revise', { maxRevisions: 1 }, [write('no'), submit, write('ok then'), submit, done]);
  assert.match(r.seen[2], /test\.note v1: \d+% \(says-ok@1\), failed:.*Revise the file to address each point, then call submit_note again\./);
  assert.match(r.seen[2], /"isError":true/);
  assert.match(r.seen[4], /test\.note v2: 100% \(says-ok@1\), passed\. Version 2 is final for this run; finish without revising it\./);
  assert.deepEqual(r.versions.map(passed), [false, true]);
});

const firstRefusal = (outcome: string) => new RegExp(`test\\.note v2 ${outcome}, so it is final for this run and this change to artifacts/note\\.md was not sealed\\. Finish with your reply and seal nothing more`);

test('a revision after a pass is refused, the passed version stays the one delivered, and the writer\'s reply is the answer', async () => {
  const r = await run('after-pass', { maxRevisions: 2 }, [write('no'), submit, write('ok'), submit, write('ok, and better'), submit, fauxAssistantMessage('Delivered v2.')]);
  assert.match(r.seen[6], firstRefusal('passed and is delivered'));
  assert.match(r.seen[6], /"isError":true/);
  assert.equal(r.answer, 'Delivered v2.');
  assert.deepEqual(r.versions.map(passed), [false, true]);
});

test('a second refused revision after a pass ends the run, with the passed version still the newest', async () => {
  const r = await run('after-pass-twice', { maxRevisions: 2 }, [write('no'), submit, write('ok'), submit, write('ok, and better'), submit, write('ok, best'), submit, fauxAssistantMessage('should not be asked')]);
  assert.equal(r.asked, 8, 'no request after the second refusal');
  assert.match(r.transcript, /test\.note v2 passed and is delivered\. A second change after it was not sealed either, so this run is over\./);
  assert.deepEqual(r.versions.map(passed), [false, true]);
});

test("a person's change request after a pass is judged, with a fresh revision budget", async () => {
  const steer = async (s: Session) => (await s.submit('Make it say more.', 'steer-1', false, 'steer'), write('nope, longer'));
  const r = await run('change', { maxRevisions: 1 }, [write('no'), submit, write('ok'), submit, steer, submit, write('ok, longer'), submit, done]);
  assert.match(r.seen[5], /Make it say more\./);
  assert.match(r.seen[6], /test\.note v3: \d+% \(says-ok@1\), failed:.*Revise the file/, 'v3 is judged, and the limit counts from the pass');
  assert.deepEqual(r.versions.map(passed), [false, true, false, true]);
});

test('a version still failing at the revision limit is released with its failures marked', async () => {
  const r = await run('limit', { maxRevisions: 0 }, [write('no'), submit, done]);
  assert.match(r.seen[2], /failed:.*The revision limit is reached\. Deliver version 1 with these failures marked\./);
  assert.deepEqual(r.versions.map((v) => (v.verdict as { limitReached: boolean }).limitReached), [true]);
});

test('a revision after the limit released a version is refused, so the limit holds for the run', async () => {
  const r = await run('past', { maxRevisions: 1 }, [write('no'), submit, write('still no'), submit, write('ok'), submit, done]);
  assert.match(r.seen[4], /The revision limit is reached\. Deliver version 2 with these failures marked\./);
  assert.match(r.seen[6], firstRefusal('is delivered'));
  assert.deepEqual(r.versions.map(passed), [false, false], 'nothing past the released version is sealed');
});

test('the gate judges with item reuse on: a model item unchanged since the last version is not asked again', async () => {
  // v1 passes its gate and fails on length alone, so its model check runs.
  const long = { id: 'long', label: 'Too short', weight: 1, code: (a: string) => ({ met: a.length > 2 ? 1 : 0 }) };
  const polite = { ...sayOk, criteria: [...sayOk.criteria, { id: 'manner', checks: [{ id: 'polite', label: 'Is polite', weight: 1 }, long] }] };
  const judging: Judged = {
    rubric: polite, inputs: async () => ({ intro: 'Hello there.' }),
    declaration: { scopes: { intro: { split: { name: 'whole', from: 'intro' } } }, checks: { polite: { scope: 'intro', question: { type: 'bool', instructions: 'Is it polite?' }, aggregate: { name: 'all' } } } },
  };
  const yes = fauxAssistantMessage('{"choice": "yes", "confidence": 0.9, "reason": "greets"}');
  // The agent's model answers the escalation once; asked again on v2, it would read the final reply as the judge's answer.
  const r = await run('reuse', { maxRevisions: 1 }, [write('ok'), submit, yes, write('ok then'), submit, done], undefined, undefined, judging);
  const checks = r.versions.map((v) => (v.verdict as { checks: Array<{ id: string; resolvers: object; reused?: object }> }).checks.find((c) => c.id === 'polite'));
  assert.deepEqual(r.versions.map(passed), [false, true]);
  assert.deepEqual(checks.map((c) => [c?.resolvers, c?.reused]), [[{ 'faux-1': 1 }, undefined], [{}, { intro: 1 }]]);
  assert.equal(r.answer, 'Done.');
});

// A note whose politeness model check runs at the seal, once its says-ok gate passes.
const gatedPolite: Judged = {
  rubric: { ...sayOk, criteria: [...sayOk.criteria, { id: 'manner', checks: [{ id: 'polite', label: 'Is not polite', weight: 1 }] }] }, inputs: async () => ({ intro: 'Hello there.' }),
  declaration: { scopes: { intro: { split: { name: 'whole', from: 'intro' } } }, checks: { polite: { scope: 'intro', question: { type: 'bool', instructions: 'Is it polite?' }, aggregate: { name: 'all' } } } },
};
const v = (x: { verdict: unknown }) => x.verdict as { passed: boolean; limitReached: boolean; cap?: string; checks: Array<{ id: string; deferred?: string; resolvers: object }> };

test('a round stopped at a failed code gate spends no revision: with none left, the next version still runs its model checks', async () => {
  const yes = fauxAssistantMessage('{"choice": "yes", "confidence": 0.9, "reason": "greets"}');
  const r = await run('gate-budget', { maxRevisions: 0 }, [write('no'), submit, write('ok'), submit, yes, done], undefined, undefined, gatedPolite);
  assert.deepEqual(r.versions.map((x) => [v(x).passed, v(x).limitReached]), [[false, false], [true, false]]);
  assert.equal(v(r.versions[0]).checks.find((c) => c.id === 'polite')?.deferred, 'gate');
  assert.match(r.seen[2], /manner\.polite not run because a gate failed.*Revise the file/s);
  assert.deepEqual(v(r.versions[1]).checks.find((c) => c.id === 'polite')?.resolvers, { 'faux-1': 1 });
});

test('a writer stuck on a failed gate is released at the gate limit, and the verdict says so', async () => {
  const stuck = Array.from({ length: 5 }, (_, i) => [write(`no ${i + 1}`), submit]).flat();
  const r = await run('gate-cap', { maxRevisions: 0 }, [...stuck, done], undefined, undefined, gatedPolite);
  assert.deepEqual(r.versions.map((x) => v(x).cap ?? null), [null, null, null, null, 'gates']);
  assert.match(r.seen[10], /The gate limit is reached: 5 versions in a row failed a code gate, so no model check ran\. Deliver version 5/);
  assert.equal(r.answer, 'Done.');
});

// Resolves with the first value `pick` finds in the session's approvals, watching until one appears.
async function untilApprovals<T>(s: Session, pick: (items: Record<string, Approval>) => T | undefined): Promise<T> {
  await s.root.commit(async (tx) => { await tx.doc(Approvals, s.root.id); }, BACKGROUND_CONTEXT);
  const watch = (await s.harness.watchDoc(Approvals, s.root.id, BACKGROUND_CONTEXT))!;
  return new Promise<T>((resolve) => {
    const hit = () => pick(watch.value?.items ?? {});
    if (hit() !== undefined) return resolve(hit()!);
    watch.start(async () => { const h = hit(); if (h !== undefined) resolve(h); });
  }).finally(() => watch.stop());
}

// A note kind whose politeness check runs only on approve, and a gated tool that publishes the sealed version 1.
const polite = { ...sayOk, criteria: [...sayOk.criteria, { id: 'manner', checks: [{ id: 'polite', label: 'Is not polite', weight: 1, when: 'approval' as const }] }] };
const onApprove: Judged = {
  rubric: polite, inputs: async () => ({ intro: 'Hello there.' }),
  declaration: { scopes: { intro: { split: { name: 'whole', from: 'intro' } } }, checks: { polite: { scope: 'intro', question: { type: 'bool', instructions: 'Is it polite?' }, aggregate: { name: 'all' } } } },
};
const publish = gated(defineTool({ name: 'publish_note', description: 'Publish the note', parameters: obj({}, { open: true }), async execute() { return { output: [{ type: 'text', text: 'published' }] }; } }) as ToolRegistration, { subject: () => ({ kind: note.kind, version: 1 }) });
const publishing = [defineExtension({ name: 'publish', tools: [publish] })];
const jf = { kind: 'person', name: 'jf' } as const;
const askToPublish = (approver: (s: Session) => Promise<void>) => async (s: Session) => (void approver(s), fauxAssistantMessage(fauxToolCall('publish_note', {}), { stopReason: 'toolUse' }));

test("a check declared for approval skips every seal and runs on a person's approve of the version, for any kind with a gated tool", async () => {
  let held: Approval['checks'];
  // The person approves, reads the findings the gate held it open with, and approves again over them.
  const approver = async (s: Session) => {
    const id = await untilApprovals(s, (items) => Object.keys(items)[0]);
    await resolveApproval(s.root, id, { decision: 'approve', by: jf }, BACKGROUND_CONTEXT);
    held = await untilApprovals(s, (items) => items[id].checks ?? undefined);
    await resolveApproval(s.root, id, { decision: 'approve', by: jf }, BACKGROUND_CONTEXT);
  };
  const rude = fauxAssistantMessage('{"choice": "no", "confidence": 0.9, "reason": "no greeting"}');
  // The judge model is the agent's own: asked once, on the approve, never at the seal.
  const r = await run('on-approve', { maxRevisions: 1 }, [write('ok'), submit, askToPublish(approver), rude, done], undefined, undefined, onApprove, publishing);
  const draft = r.versions[0].verdict as { passed: boolean; checks: Array<{ id: string; met: unknown; deferred?: string }> };
  assert.deepEqual([draft.passed, draft.checks.map((c) => [c.id, c.met, c.deferred])], [true, [['says_ok', 1, undefined], ['polite', null, 'approval']]]);
  assert.match(r.seen[2], /manner\.polite not run yet, they run when a person approves\), passed\./);
  const detail = held?.detail as { passed: boolean; checks: Array<{ id: string; met: unknown }> };
  assert.deepEqual([held?.passed, detail.passed, detail.checks.find((c) => c.id === 'polite')?.met], [false, false, false]);
  assert.match(held!.feedback.join(''), /missed manner\.polite: intro: no greeting/);
  assert.match(r.seen[4], /published/);
  assert.equal(r.answer, 'Done.');
});

test('when no model answers on approve, the checks are undecided, not failed, and the next plain approve runs them again', async () => {
  let first: Approval['checks'];
  let second: Approval['checks'];
  const approver = async (s: Session) => {
    const id = await untilApprovals(s, (items) => Object.keys(items)[0]);
    await resolveApproval(s.root, id, { decision: 'approve', by: jf }, BACKGROUND_CONTEXT);
    first = await untilApprovals(s, (items) => items[id].checks ?? undefined);
    await resolveApproval(s.root, id, { decision: 'approve', by: jf }, BACKGROUND_CONTEXT);
    second = await untilApprovals(s, (items) => (items[id].verdict ? items[id].checks ?? undefined : undefined));
  };
  // The judge model's first answer is unusable, so the item stays undecided; asked again on the retry, it answers.
  const garbled = fauxAssistantMessage('the model is overloaded');
  const polite = fauxAssistantMessage('{"choice": "yes", "confidence": 0.9, "reason": "greets"}');
  const r = await run('on-approve-retry', { maxRevisions: 1 }, [write('ok'), submit, askToPublish(approver), garbled, polite, done], undefined, undefined, onApprove, publishing);
  assert.deepEqual([first?.passed, first?.decided, first?.detail], [false, false, null]);
  assert.match(first!.feedback.join(''), /gave no usable answer for manner\.polite/);
  assert.deepEqual([second?.passed, second?.decided], [true, true], 'the retry ran the checks and they passed');
  assert.match(r.seen[5], /published/);
  assert.equal(r.answer, 'Done.');
});

test('a refusal from the registration ends the run before anything is judged', async () => {
  const r = await run('refuse', {}, [write('ok'), submit, fauxAssistantMessage('should not be asked')], async () => 'Nothing to submit: no work. This run is over.');
  assert.match(r.transcript, /Nothing to submit: no work\. This run is over\./);
  assert.equal(r.asked, 2);
  assert.deepEqual(r.versions.map(passed), [null]);
});

test('a run with nothing to submit may finish without sealing', async () => {
  const r = await run('refuse-yield', {}, [done, fauxAssistantMessage('should not be asked')], async () => 'Nothing to submit: no work. This run is over.');
  assert.equal(r.asked, 1, 'the yield hook does not push for a submit the registration would refuse');
  assert.deepEqual(r.versions, []);
});

test('a run may not finish before its kind is sealed and checked', async () => {
  const r = await run('yield', { maxRevisions: 1 }, [done, write('ok'), submit, done]);
  assert.match(r.seen[1], /Write artifacts\/note\.md and call submit_note before you finish\./);
  assert.deepEqual(r.versions.map(passed), [true]);
});

test('a subagent child conversation can yield without sealing while the root conversation is still gated', async () => {
  const seen: string[] = [];
  let childCalls = 0;
  let rootStep = 0;
  const rootSteps = [
    fauxAssistantMessage(fauxToolCall('subagent', { task: 'gather facts' }), { stopReason: 'toolUse' }),
    done,
    write('ok'),
    submit,
    done,
  ];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(Array.from({ length: 12 }, () => (c: { messages: Array<{ role: string; content: unknown }> }) => {
    const all = JSON.stringify(c.messages);
    const last = JSON.stringify(c.messages.at(-1));
    seen.push(last);
    if (all.includes('gather facts') && !all.includes('Write and submit the note.')) {
      childCalls++;
      return fauxAssistantMessage('gathered facts');
    }
    return rootSteps[rootStep++] ?? done;
  }));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const judge = await manifest.session!({ maxRevisions: 1 }, {}, []);
  judged(note.kind, how());
  const session = await openSession({ dir: join(dir, 'subagent-yield'), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, ...judge, artifactsExtension([note]), subagentExtension()] } });
  const result = await session.submit('Write and submit the note.', 'subagent-yield-1', true);
  const versions = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))?.kinds[note.kind]?.versions ?? [];
  await session.close();
  assert.equal(childCalls, 1, 'subagent yields on its first answer without being trapped by yieldHook');
  assert.ok(seen.some((m) => m.includes('Write artifacts/note.md and call submit_note before you finish.')), 'root conversation is still gated');
  assert.equal(result.status === 'done' && result.answer, 'Done.');
  assert.deepEqual(versions.map(passed), [true]);
});

test('a later run may not finish on a passed version written against an earlier basis', async () => {
  head = 'a';
  const moved = async () => ((head = 'b'), done);
  const r = await run('basis', {}, [write('ok'), submit, done, moved, write('ok again'), submit, done], undefined, ['Review a.', 'Review b.']).finally(() => (head = undefined));
  assert.match(r.seen[4], /test\.note v1 was written against a, not b\. Write artifacts\/note\.md for b and call submit_note before you finish\./);
  assert.deepEqual(r.versions.map((v) => [v.basis, passed(v)]), [['a', true], ['b', true]]);
});

test('the manifest names the judge and reports what is wrong with an agent\'s judge config', () => {
  assert.equal(manifest.name, 'judge');
  const problems = (c: unknown) => manifest.validate!(c, { providers: {} });
  assert.deepEqual(problems({ maxRevisions: 2, decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'local', modelId: 'm' } }), []);
  assert.deepEqual(problems({ maxRevisions: -1, decisionModel: { provider: 'clef' }, retries: 1 }), [
    'judge.retries is not a judge setting; known: maxRevisions, decisionModel, judgeModel',
    'judge.maxRevisions must be a non-negative integer',
    'judge.decisionModel must be { provider, modelId, maxInputTokens }',
  ]);
});

import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts, artifactsExtension, type ArtifactKind } from '@quack/sdk';
import { openSession, readExport } from '@quack/session';
import { judged, manifest, type JudgeConfig, type Judged } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-judge-ext-'));
after(() => rm(dir, { recursive: true, force: true }));

const note: ArtifactKind = { kind: 'test.note', path: 'artifacts/note.md', tool: 'submit_note' };
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
async function run(name: string, config: JudgeConfig, script: Step[], refuse?: Judged['refuse']) {
  const seen: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  let session: Session;
  faux.setResponses(script.map((step) => async (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), typeof step === 'function' ? step(session) : step)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const judge = await manifest.session!(config, {}, []);
  judged(note.kind, how(refuse));
  session = await openSession({ dir: join(dir, name), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, ...judge, artifactsExtension([note])] } });
  const result = await session.submit('Write and submit the note.', `${name}-1`, true);
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

test('the manifest names the judge and reports what is wrong with an agent\'s judge config', () => {
  assert.equal(manifest.name, 'judge');
  const problems = (c: unknown) => manifest.validate!(c, { providers: {} });
  assert.deepEqual(problems({ maxRevisions: 2, decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'local', modelId: 'm' }, escalations: 4 }), []);
  assert.deepEqual(problems({ maxRevisions: -1, decisionModel: { provider: 'clef' }, retries: 1 }), [
    'judge.retries is not a judge setting; known: maxRevisions, decisionModel, judgeModel, escalations',
    'judge.maxRevisions must be a non-negative integer',
    'judge.decisionModel must be { provider, modelId, maxInputTokens }',
  ]);
});

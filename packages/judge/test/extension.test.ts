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

// One run of an agent that enables the judge with `config` and registers test.note, the way a feature's session setup does.
async function run(name: string, config: JudgeConfig, script: Array<ReturnType<typeof fauxAssistantMessage>>, refuse?: Judged['refuse']) {
  const seen: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(script.map((reply) => (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), reply)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const judge = await manifest.session!(config, {}, []);
  judged(note.kind, how(refuse));
  const session = await openSession({ dir: join(dir, name), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, ...judge, artifactsExtension([note])] } });
  await session.submit('Write and submit the note.', `${name}-1`, true);
  const versions = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))?.kinds[note.kind]?.versions ?? [];
  await session.close();
  const transcript = JSON.stringify((await readExport(join(dir, name))).transcript);
  return { seen, versions, transcript, asked: seen.length };
}

const passed = (v: { verdict: unknown }) => (v.verdict as { passed?: boolean } | null)?.passed ?? null;

test('a failed version comes back to revise; the passing one seals with the judge report as its note', async () => {
  const r = await run('revise', { maxRevisions: 1 }, [write('no'), submit, write('ok then'), submit, done]);
  assert.match(r.seen[2], /test\.note v1: \d+% \(says-ok@1\), failed:.*Revise the file to address each point, then call submit_note again\./);
  assert.match(r.seen[2], /"isError":true/);
  assert.match(r.seen[4], /test\.note v2: 100% \(says-ok@1\), passed\. Deliver version 2\./);
  assert.deepEqual(r.versions.map(passed), [false, true]);
});

test('a version still failing at the revision limit is released with its failures marked', async () => {
  const r = await run('limit', { maxRevisions: 0 }, [write('no'), submit, done]);
  assert.match(r.seen[2], /failed:.*The revision limit is reached\. Deliver version 1 with these failures marked\./);
  assert.deepEqual(r.versions.map((v) => (v.verdict as { limitReached: boolean }).limitReached), [true]);
});

test('a seal past the limit is refused and ends the run', async () => {
  const r = await run('past', { maxRevisions: 0 }, [write('no'), submit, write('still no'), submit, fauxAssistantMessage('should not be asked')]);
  assert.match(r.transcript, /test\.note was already checked 1 times in this run, the most it gets\. Stop: this run is over\./);
  assert.equal(r.asked, 4, 'no request after the refused seal');
  assert.deepEqual(r.versions.map(passed), [false, null], 'the refused version is sealed but not judged');
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

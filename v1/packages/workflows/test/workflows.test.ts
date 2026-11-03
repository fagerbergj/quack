import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, type FauxResponseFactory, type Message } from '@earendil-works/pi-ai';
import { num, obj, str } from '@earendil-works/pi-schema';
import { defineTool } from '@earendil-works/pi-durable';
import { answerQuestion, Approvals, Artifacts, Failures, pendingApprovals, pendingQuestions, Questions, resolveApproval, seal, type ArtifactKind } from '@quack/sdk';
import { openSession } from '@quack/session';
import { defineWorkflow, outcomeOf, resumeWorkflow, runWorkflow, startWorkflow, watchUntil, WorkflowDoc, workflowExtension, type Workflow } from '../src/index.ts';

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));
const tmp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-wf-')); dirs.push(d); return d; };

function open(dir: string, workflow: Workflow, ...replies: (string | FauxResponseFactory)[]) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(replies.map((r) => (typeof r === 'string' ? fauxAssistantMessage(r) : r)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [workflowExtension([workflow])] } });
}

type Open = Awaited<ReturnType<typeof open>>;
const doc = async (s: Open) => (await s.harness.snapshot(WorkflowDoc, s.root.id, ctx))!;
const waitFor = async (s: Open, pick: Parameters<typeof watchUntil>[1]) => watchUntil((await s.harness.watchDoc(WorkflowDoc, s.root.id, ctx))!, pick);

// The first open question; touching the document first lets it be watched before anything has asked.
async function asked(s: Open) {
  await s.root.commit(async (tx) => { await tx.doc(Questions, s.root.id); }, ctx);
  const watch = (await s.harness.watchDoc(Questions, s.root.id, ctx))!;
  return new Promise<ReturnType<typeof pendingQuestions>>((resolve) => {
    const open = () => pendingQuestions(watch.value);
    if (open().length) return resolve(open());
    watch.start(async () => { if (open().length) resolve(open()); });
  }).finally(() => watch.stop());
}

const note: ArtifactKind = { kind: 'test.note', path: 'note.md' };

test('a code, model and code workflow completes, records its steps and seals its artifact', async () => {
  const workflow = defineWorkflow({
    name: 'three',
    steps: [
      { name: 'gather', kind: 'code', run: async (c) => { await c.api.env!.writeFile('note.md', 'ducks', ctx); await seal(c.api, note, ctx); return { items: [1, 2, 3] }; } },
      { name: 'rank', kind: 'model', schema: obj({ top: num() }, { open: true }), prompt: (c) => `Pick the best of ${JSON.stringify(c.prior.gather)}` },
      { name: 'report', kind: 'code', run: (c) => `top is ${(c.prior.rank as { top: number }).top}` },
    ],
  });
  const s = await open(await tmp(), workflow, 'Sure: {"top": 3}');
  const outcome = await runWorkflow(s.harness, s.root, workflow, null);

  assert.equal(outcome.status, 'completed');
  assert.deepEqual(outcome.status === 'completed' && outcome.result, { gather: { items: [1, 2, 3] }, rank: { top: 3 }, report: 'top is 3' });
  const steps = (await doc(s)).steps;
  assert.deepEqual(steps.map((r) => [r.name, r.kind, r.status]), [['gather', 'code', 'done'], ['rank', 'model', 'done'], ['report', 'code', 'done']]);
  assert.ok(steps.every((r) => r.startedAt !== null && r.durationMs !== null && r.durationMs >= 0));
  const sealed = await s.harness.snapshot(Artifacts, s.root.id, ctx);
  assert.equal(sealed!.kinds['test.note'].versions[0].content, 'ducks');
  await s.close();
});

test('a crash mid-step resumes at that step without rerunning earlier ones', async () => {
  const runs = { first: 0, second: 0 };
  let entered: () => void;
  const inStep = new Promise<void>((r) => (entered = r));
  const build = (hang: boolean) => defineWorkflow({
    name: 'resume',
    steps: [
      { name: 'first', kind: 'code', run: () => ++runs.first },
      {
        name: 'second',
        kind: 'code',
        run: async (c) => {
          runs.second++;
          if (!hang) return 'finished';
          entered();
          await new Promise((_, reject) => c.signal.addEventListener('abort', () => reject(c.signal.reason)));
          return 'unreachable';
        },
      },
    ],
  });
  const dir = await tmp();
  const a = await open(dir, build(true));
  const running = runWorkflow(a.harness, a.root, build(true), null).catch(() => undefined);
  await inStep;
  await a.close();
  await running;

  const b = await open(dir, build(false));
  const outcome = await runWorkflow(b.harness, b.root, build(false), null);
  assert.deepEqual(outcome.status === 'completed' && outcome.result, { first: 1, second: 'finished' });
  assert.deepEqual(runs, { first: 1, second: 2 });
  assert.deepEqual((await doc(b)).steps.map((r) => r.status), ['done', 'done']);
  await b.close();
});

test('a model reply that fails the schema fails the step and stops the workflow', async () => {
  let ranAfter = false;
  const workflow = defineWorkflow({
    name: 'strict',
    steps: [
      { name: 'rank', kind: 'model', schema: obj({ top: num() }, { open: true }), prompt: () => 'rank' },
      { name: 'after', kind: 'code', run: () => { ranAfter = true; return null; } },
    ],
  });
  const bad = '{"top": "three"}';
  const s = await open(await tmp(), workflow, bad, bad, bad);
  const outcome = await runWorkflow(s.harness, s.root, workflow, null);

  assert.equal(outcome.status, 'failed');
  const [rank, after] = (await doc(s)).steps;
  assert.equal(rank.status, 'failed');
  assert.match(rank.error!, /step rank:.*top/s);
  assert.equal(after.status, 'pending');
  assert.equal(ranAfter, false);
  await s.close();
});

const capped = defineWorkflow({
  name: 'capped',
  steps: [{ name: 'decide', kind: 'model', schema: obj({ summary: str({ maxLength: 5 }) }, { open: true }), prompt: () => 'summarize' }],
});

test('resuming a failed run reruns the failed step and later ones, keeping the earlier results', async () => {
  const runs = { first: 0, second: 0, third: 0 };
  let broken = true;
  const workflow = defineWorkflow({
    name: 'retry',
    steps: [
      { name: 'first', kind: 'code', run: () => ({ n: ++runs.first }) },
      { name: 'second', kind: 'code', run: () => { runs.second++; if (broken) throw new Error('boom'); return 'fixed'; } },
      { name: 'third', kind: 'code', run: (c) => { runs.third++; return JSON.stringify(c.prior); } },
    ],
  });
  const s = await open(await tmp(), workflow);
  assert.equal((await runWorkflow(s.harness, s.root, workflow, 'in')).status, 'failed');
  assert.equal((await doc(s)).steps[1].status, 'failed');
  assert.equal((await s.harness.snapshot(Failures, s.root.id, ctx))!.items.second.error, 'boom');

  broken = false;
  const outcome = await outcomeOf(s.harness, (await resumeWorkflow(s.harness, s.root, workflow))!);
  assert.deepEqual(outcome.status === 'completed' && outcome.result, { first: { n: 1 }, second: 'fixed', third: '{"first":{"n":1},"second":"fixed"}' });
  assert.deepEqual(runs, { first: 1, second: 2, third: 1 });
  assert.deepEqual((await doc(s)).steps.map((r) => [r.status, r.error]), [['done', null], ['done', null], ['done', null]]);
  assert.equal((await s.harness.snapshot(Failures, s.root.id, ctx))!.items.second.error, null);
  assert.equal(await resumeWorkflow(s.harness, s.root, workflow), null);
  await s.close();
});

test('a failed run with no kept results resumes at the first step that has none and completes', async () => {
  const runs = { first: 0, second: 0 };
  let broken = true;
  const workflow = defineWorkflow({
    name: 'nokept',
    steps: [
      { name: 'first', kind: 'code', run: () => ({ n: ++runs.first }) },
      { name: 'second', kind: 'code', run: (c) => { runs.second++; if (broken) throw new Error('boom'); return JSON.stringify(c.prior); } },
    ],
  });
  const s = await open(await tmp(), workflow);
  assert.equal((await runWorkflow(s.harness, s.root, workflow, null)).status, 'failed');
  // A run that failed before `kept` was recorded.
  await s.root.commit(async (tx) => { (await tx.doc(WorkflowDoc, s.root.id)).kept = null; }, ctx);

  broken = false;
  const outcome = await outcomeOf(s.harness, (await resumeWorkflow(s.harness, s.root, workflow))!);
  assert.deepEqual(outcome.status === 'completed' && outcome.result, { first: { n: 2 }, second: '{"first":{"n":2}}' });
  assert.deepEqual(runs, { first: 2, second: 2 });
  await s.close();
});

// Answers with `text` and records the messages each request carried.
const reply = (text: string, seen: Message[][]): FauxResponseFactory => (context) => {
  seen.push([...context.messages]);
  return fauxAssistantMessage(text);
};

test('a model reply that breaks the schema is repaired with the error and the first reply', async () => {
  const seen: Message[][] = [];
  const s = await open(await tmp(), capped, reply('{"summary": "far too long"}', seen), reply('{"summary": "ok"}', seen));
  const outcome = await runWorkflow(s.harness, s.root, capped, null);

  assert.equal(outcome.status, 'completed');
  assert.equal(seen.length, 2);
  assert.deepEqual(seen[1].map((m) => m.role), ['user', 'assistant', 'user']);
  assert.match(JSON.stringify(seen[1][1]), /far too long/);
  assert.match(JSON.stringify(seen[1][2]), /summary.*expected at most 5 characters/s);
  await s.close();
});

test('a model step that keeps breaking its schema fails with the last error after two repairs', async () => {
  const seen: Message[][] = [];
  const s = await open(await tmp(), capped, reply('{"summary": "too long 1"}', seen), reply('{"summary": "too long 2"}', seen), reply('{"summary": "too long 3"}', seen));
  const outcome = await runWorkflow(s.harness, s.root, capped, null);

  assert.equal(outcome.status, 'failed');
  assert.equal(seen.length, 3);
  assert.match((await doc(s)).steps[0].error!, /step decide:.*summary/s);
  await s.close();
});

test('a model reply that is valid the first time is asked for once', async () => {
  const seen: Message[][] = [];
  const s = await open(await tmp(), capped, reply('{"summary": "ok"}', seen), reply('{"summary": "again"}', seen));
  const outcome = await runWorkflow(s.harness, s.root, capped, null);

  assert.equal(outcome.status, 'completed');
  assert.equal(seen.length, 1);
  await s.close();
});

test('a decision step waits for a person across a restart and resumes on the answer', async () => {
  const build = () => defineWorkflow({
    name: 'decide',
    steps: [
      {
        name: 'pick',
        kind: 'decision',
        point: () => ({ question: 'Who starts?', options: ['Pollard', 'Stevenson'] }),
        resolvers: [{ id: 'unsure', resolve: async () => undefined }],
      },
      { name: 'announce', kind: 'code', run: (c) => `start ${(c.prior.pick as { choice: string }).choice}` },
    ],
  });
  const dir = await tmp();
  const a = await open(dir, build());
  const id = await startWorkflow(a.harness, a.root, build(), null); // the document exists once the run does
  const running = outcomeOf(a.harness, id).catch(() => undefined);
  const [ask] = await asked(a);
  assert.equal(ask.question, 'Who starts?');
  assert.equal((await doc(a)).steps[0].status, 'waiting');
  await a.close();
  await running;

  const b = await open(dir, build());
  assert.equal(await answerQuestion(b.root, ask.id, 'Maybe', 'jf', ctx), 'bad-choice', 'only a listed option answers');
  assert.equal(await answerQuestion(b.root, ask.id, { text: 'Maybe' }, 'jf', ctx), 'bad-choice', 'a decision step takes labels only');
  assert.equal(await answerQuestion(b.root, ask.id, { choice: 'Pollard', note: 'x' }, 'jf', ctx), 'bad-choice', 'and no note');
  assert.equal(await answerQuestion(b.root, ask.id, { text: 'Maybe' }, 'jf', ctx), 'bad-choice', 'a decision step takes labels only');
  assert.equal(await answerQuestion(b.root, ask.id, { choice: 'Pollard', note: 'x' }, 'jf', ctx), 'bad-choice', 'and no note');
  assert.equal(await answerQuestion(b.root, ask.id, 'Pollard', 'jf', ctx), 'ok');
  assert.equal(await answerQuestion(b.root, ask.id, 'Stevenson', 'jf', ctx), 'answered', 'the first answer stands');
  const outcome = await runWorkflow(b.harness, b.root, build(), null);
  assert.equal(outcome.status === 'completed' && (outcome.result as { announce: string }).announce, 'start Pollard');
  await b.close();
});

test('a gated step waits for approval and runs its tool once with earlier results', async () => {
  const posted: string[] = [];
  const post = defineTool({
    name: 'post_comment',
    description: 'Post a comment',
    parameters: obj({ body: str() }, { open: true }),
    execute: async (args) => { posted.push(args.body); return { output: [{ type: 'text', text: 'posted' }] }; },
  });
  const workflow = defineWorkflow({
    name: 'gate',
    steps: [
      { name: 'draft', kind: 'code', run: () => 'LGTM' },
      { name: 'post', kind: 'gated', tool: post, args: (c) => ({ body: c.prior.draft as string }) },
    ],
  });
  const s = await open(await tmp(), workflow);
  const running = runWorkflow(s.harness, s.root, workflow, null);
  await pendingApprovals(s.root, ctx); // creates the document so it can be watched
  const watch = (await s.harness.watchDoc(Approvals, s.root.id, ctx))!;
  const id = await new Promise<string>((resolve) => {
    const find = () => Object.keys(watch.value?.items ?? {})[0];
    if (find()) return resolve(find());
    watch.start(async () => { if (find()) resolve(find()); });
  });
  await watch.stop();
  assert.deepEqual(posted, []);
  assert.equal(await resolveApproval(s.root, id, { decision: 'approve', by: 'jf' }, ctx), true);

  const outcome = await running;
  assert.equal(outcome.status, 'completed');
  assert.deepEqual(posted, ['LGTM']);
  await s.close();
});

const sayOk = { name: 'says-ok', version: 1, threshold: 1, criteria: [{ id: 'tone', checks: [{ id: 'says_ok', label: 'Does not say ok', weight: 'gate' as const, code: (a: string) => ({ met: a.includes('ok') ? 1 : 0, detail: 'the text must say ok' }) }] }] };
const judgedFlow = defineWorkflow({
  name: 'judged',
  steps: [{
    name: 'card',
    kind: 'judged',
    schema: obj({ text: str() }, { open: true }),
    prompt: () => 'write the text',
    card: (_c, d) => d as object,
    judge: () => ({ options: { kind: note, rubric: sayOk, evidence: async () => ({}), maxRevisions: 1 }, notes: ['n'] }),
  }],
});

async function runJudged(...replies: FauxResponseFactory[]) {
  const s = await open(await tmp(), judgedFlow, ...replies);
  const outcome = await runWorkflow(s.harness, s.root, judgedFlow, null);
  const versions = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['test.note'].versions;
  await s.close();
  return { outcome, versions };
}

test('a judged card that fails is revised once with its own answer and the judge\'s notes, and sealed as v2', async () => {
  const seen: Message[][] = [];
  const { outcome, versions } = await runJudged(reply('{"text": "fine"}', seen), reply('{"text": "ok"}', seen));

  assert.equal(outcome.status, 'completed');
  assert.deepEqual(versions.map((v) => [v.version, (v.verdict as { passed: boolean }).passed]), [[1, false], [2, true]]);
  assert.deepEqual(seen[1].map((m) => m.role), ['user', 'assistant', 'user']);
  assert.match(JSON.stringify(seen[1][1]), /fine/);
  assert.match(JSON.stringify(seen[1][2]), /tone\.says_ok.*the text must say ok/s);
});

test('a judged card that passes is not revised', async () => {
  const seen: Message[][] = [];
  const { versions } = await runJudged(reply('{"text": "ok"}', seen));
  assert.equal(seen.length, 1);
  assert.equal(versions.length, 1);
});

test('a judged card still failing after the last revision is delivered with its failing verdict', async () => {
  const seen: Message[][] = [];
  const { outcome, versions } = await runJudged(reply('{"text": "no"}', seen), reply('{"text": "nope"}', seen), reply('{"text": "never"}', seen));

  assert.equal(seen.length, 2);
  assert.deepEqual(versions.map((v) => (v.verdict as { passed: boolean }).passed), [false, false]);
  assert.equal((versions[1].verdict as { limitReached: boolean }).limitReached, true);
  assert.deepEqual(outcome.status === 'completed' && (outcome.result as { card: unknown }).card, { version: 2, revisions: 1, passed: false, score: 1, gatesFailed: ['tone.says_ok'], notes: ['n'] });
});

test('a judged step whose judge model gives no answer fails the step and attaches no verdict', async () => {
  const asked = { name: 'asks', version: 1, threshold: 0.5, criteria: [{ id: 'c', checks: [{ id: 'q', label: 'q', weight: 1, scope: 'review', question: 'Is it fine?' }] }] };
  const flow = defineWorkflow({
    name: 'judged-outage',
    steps: [{ name: 'card', kind: 'judged', schema: obj({ text: str() }, { open: true }), prompt: () => 'write the text', card: (_c, d) => d as object,
      judge: () => ({ options: { kind: note, rubric: asked, evidence: async () => ({ review: [{ id: 'r', state: { text: 'x' } }] }), maxRevisions: 1 } }) }],
  });
  const busy = fauxAssistantMessage('', { stopReason: 'error', errorMessage: '429 concurrency_limit' });
  const s = await open(await tmp(), flow, reply('{"text": "ok"}', []), () => busy);
  const outcome = await runWorkflow(s.harness, s.root, flow, null);
  const versions = (await s.harness.snapshot(Artifacts, s.root.id, ctx))!.kinds['test.note'].versions;
  await s.close();
  assert.equal(outcome.status, 'failed');
  assert.equal(versions[0].verdict ?? null, null, 'an outage is not a verdict');
});

function throwingFlow(builds: { n: number }, failures: number) {
  return defineWorkflow({
    name: 'built',
    steps: [{
      name: 'card',
      kind: 'judged',
      schema: obj({ text: str() }, { open: true }),
      prompt: () => 'write the text',
      card: (_c, d) => {
        if (builds.n++ < failures) throw new Error('the card builder rejects this');
        return d as object;
      },
      judge: () => ({ options: { kind: note, rubric: sayOk, evidence: async () => ({}), maxRevisions: 0 } }),
    }],
  });
}

test('a judged step whose builder throws once completes after one repair turn that shows the error', async () => {
  const seen: Message[][] = [];
  const builds = { n: 0 };
  const flow = throwingFlow(builds, 1);
  const s = await open(await tmp(), flow, reply('{"text": "ok"}', seen), reply('{"text": "ok again"}', seen));
  const outcome = await runWorkflow(s.harness, s.root, flow, null);
  await s.close();
  assert.equal(outcome.status, 'completed');
  assert.equal(builds.n, 2);
  assert.match(JSON.stringify(seen[1].at(-1)), /That answer was rejected:\\nthe card builder rejects this/);
});

test('a judged step whose builder keeps throwing fails with its message after the repair turns', async () => {
  const seen: Message[][] = [];
  const builds = { n: 0 };
  const flow = throwingFlow(builds, 99);
  const s = await open(await tmp(), flow, reply('{"text": "a"}', seen), reply('{"text": "b"}', seen), reply('{"text": "c"}', seen));
  const outcome = await runWorkflow(s.harness, s.root, flow, null);
  await s.close();
  assert.equal(outcome.status, 'failed');
  assert.match(JSON.stringify(outcome), /the card builder rejects this/);
  assert.equal(seen.length, 3);
});

test('a free-text decision takes a trimmed, bounded text or a note on a label, and the answer carries them', async () => {
  const build = () => defineWorkflow({
    name: 'free',
    steps: [{ name: 'pick', kind: 'decision', point: () => ({ question: 'Who starts?', options: ['Pollard', 'Stevenson'], freeText: true }), resolvers: [{ id: 'unsure', resolve: async () => undefined }] }],
  });
  const s = await open(await tmp(), build());
  const running = runWorkflow(s.harness, s.root, build(), null);
  const [ask] = await asked(s);
  assert.equal(await answerQuestion(s.root, ask.id, { text: '   ' }, 'jf', ctx), 'bad-choice', 'empty text');
  assert.equal(await answerQuestion(s.root, ask.id, { text: 'x'.repeat(2001) }, 'jf', ctx), 'bad-choice', 'too long');
  assert.equal(await answerQuestion(s.root, ask.id, { choice: 'Pollard', text: 'both' }, 'jf', ctx), 'bad-choice', 'choice and text together');
  assert.equal(await answerQuestion(s.root, ask.id, { choice: 'Maybe', note: 'x' }, 'jf', ctx), 'bad-choice', 'a note does not excuse an unlisted choice');
  assert.equal(await answerQuestion(s.root, ask.id, { text: '  Neither, rest both  ' }, 'jf', ctx), 'ok');
  const stored = (await s.harness.snapshot(Questions, s.root.id, ctx))!.items[ask.id]!.answer;
  assert.deepEqual([stored?.text, stored?.choice], ['Neither, rest both', 'Neither, rest both']);
  await running;
  await s.close();
});

test('a note rides with a listed choice and is trimmed', async () => {
  const build = () => defineWorkflow({
    name: 'note',
    steps: [{ name: 'pick', kind: 'decision', point: () => ({ question: 'Who starts?', options: ['Pollard', 'Stevenson'], freeText: true }), resolvers: [{ id: 'unsure', resolve: async () => undefined }] }],
  });
  const s = await open(await tmp(), build());
  const running = runWorkflow(s.harness, s.root, build(), null);
  const [ask] = await asked(s);
  assert.equal(await answerQuestion(s.root, ask.id, { choice: 'Pollard', note: '  indoor game  ' }, 'jf', ctx), 'ok');
  const stored = (await s.harness.snapshot(Questions, s.root.id, ctx))!.items[ask.id]!.answer;
  assert.deepEqual([stored?.choice, stored?.note, stored?.text], ['Pollard', 'indoor game', undefined]);
  await running;
  await s.close();
});

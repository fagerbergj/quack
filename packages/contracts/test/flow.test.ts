import assert from 'node:assert/strict';
import { manifest as judgeManifest } from '@quack/judge';
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { answerQuestion, Approvals, Artifacts, pendingApprovals, pendingQuestions, Questions, resolveApproval, SessionTitle } from '@quack/sdk';
import { buildModels, openSession, parseDeployment } from '@quack/session';
import { readSession } from '@quack/eval';
import type { Inputs } from '@quack/judge';
import { contractRecordOf, contractsSessionExtensions, manifest, specInputs, type ContractSpec } from '../src/index.ts';
import { lastReviewBlock, reviewReason } from '../src/reason.ts';
import { scheduleSpecV1, scheduleSpecV2, settingsSpec, SLEEPER_PAGE } from './fixtures.ts';

const repo = join(dirname(fileURLToPath(import.meta.url)), '../../..');
const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

type Msgs = { messages: Array<{ content: unknown }> };
type Step = (c: Msgs) => ReturnType<typeof fauxAssistantMessage>;
const call = (name: string, args: Record<string, unknown>): Step => () => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });
const text = (c: Msgs) => { const m = c.messages.at(-1)!.content; return typeof m === 'string' ? m : (m as Array<{ text?: string }>).map((p) => p.text ?? '').join(''); };
const refIn = (c: Msgs) => /ref "([^"]+)"/.exec(text(c))?.[1] ?? assert.fail(`no ref in: ${text(c)}`);
const write = (spec: () => ContractSpec): Step => (c) => call('write', { path: 'artifacts/spec.json', content: JSON.stringify(spec(), null, 2) })(c);
const jf = { kind: 'person', name: 'jf' } as const;

// A stub model: judge questions ("Decision: ..." or batched "Evidence: ...") get "yes", except the pitfall questions; everything else follows the script in order.
const judgeReply = (p: string) => {
  const answer = (q: string) => ({ choice: /implement the non-goal/.test(q) ? 'no' : 'yes', confidence: 1, reason: 'fine' });
  if (p.startsWith('Decision:')) return fauxAssistantMessage(JSON.stringify(answer(p)));
  if (p.startsWith('Evidence:')) return fauxAssistantMessage(JSON.stringify(Object.fromEntries([...p.matchAll(/^- (q\d+): ([\s\S]*?)\n  Options:/gm)].map(([, id, q]) => [id, answer(q)]))));
  return undefined;
};
async function open(script: Step[], maxRevisions = 2, inputs?: ReturnType<typeof specInputs>, reopen?: string) {
  const faux = fauxProvider({ models: [{ id: 'stub' }] });
  const queue = [...script];
  faux.setResponses(Array.from({ length: 80 }, () => (c: Msgs) => judgeReply(text(c)) ?? queue.shift()!(c)));
  const models = buildModels(parseDeployment({ defaultModel: 'unused/m', providers: { unused: { baseUrl: 'http://127.0.0.1:1/v1', models: { m: { contextWindow: 8192, maxTokens: 1024 } } } } }));
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const dir = reopen ?? (await mkdtemp(join(tmpdir(), 'quack-contracts-')));
  dirs.push(dir);
  // Before the open: a reopened session resumes its run at once, and a judge reading a file mid-copy finds no line to cite.
  for (const f of ['packages/sleeper/src/service.ts', 'packages/web/src/extensions/sleeper/api.ts']) {
    await mkdir(dirname(join(dir, 'workspace', f)), { recursive: true });
    await cp(join(repo, f), join(dir, 'workspace', f));
  }
  return openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, ...(await judgeManifest.session!({ maxRevisions }, {}, [])), ...contractsSessionExtensions(inputs)] } });
}
type Open = Awaited<ReturnType<typeof open>>;

// Resolves with the first value `pick` finds in the document; touching it first lets it be watched before anything has written it.
async function first<D extends typeof Questions | typeof Approvals, T>(s: Open, def: D, pick: (v: unknown) => T | undefined): Promise<T> {
  await s.root.commit(async (tx) => { await tx.doc(def as never, s.root.id); }, ctx);
  const watch = (await s.harness.watchDoc(def as never, s.root.id, ctx))!;
  return new Promise<T>((resolve) => {
    const hit = () => pick(watch.value);
    if (hit() !== undefined) return resolve(hit()!);
    watch.start(async () => { if (hit() !== undefined) resolve(hit()!); });
  }).finally(() => watch.stop());
}
const nextQuestion = (s: Open, seen: string[] = []) => first(s, Questions, (v) => {
  const fresh = pendingQuestions(v as never).filter((q) => !seen.includes(q.id));
  return fresh.length ? fresh : undefined;
});
type Proposal = { id: string; args: { summary: string; version: number; artifact: number; hash: string; findings: unknown[]; breaks: unknown[] } };
const nextProposal = (s: Open, seen: string[] = []) => first(s, Approvals, (v) => {
  const [id, a] = Object.entries((v as { items?: Record<string, { verdict: unknown; args: unknown }> } | null)?.items ?? {}).find(([k, x]) => !x.verdict && !seen.includes(k)) ?? [];
  return id ? ({ id, args: a!.args } as Proposal) : undefined;
});
const approve = async (s: Open, seen: string[] = []) => {
  const p = await nextProposal(s, seen);
  assert.equal(await resolveApproval(s.root, p.id, { decision: 'approve', by: jf }, ctx), true);
  return p;
};
const versions = async (s: Open) => (await s.harness.snapshot(Artifacts, s.root.id, ctx))?.kinds['contract.spec']?.versions ?? [];
const kindOf = async (s: Open, kind: string) => (await s.harness.snapshot(Artifacts, s.root.id, ctx))?.kinds[kind]?.versions ?? [];
const sessionTitle = async (s: Open) => (await s.harness.snapshot(SessionTitle, s.root.id, ctx))?.title ?? undefined;

const refs = { versioning: '', time: '' };
const received = { versioning: '', time: '' };
const askVersioning = call('ask_question', { id: 'versioning', header: 'Versioning', title: 'Keep the route or add `/v1`?', detail: 'The Sleeper page calls the route today.', suggestedTradeoff: 'No client change.', question: 'Keep the existing unversioned route, or add /v1?', matters: 'A new major version is a new contract for every caller.', suggested: 'Keep the existing unversioned route', alternatives: [{ answer: 'Add /v1 (AIP-185)', tradeoff: 'Room for a breaking change later, but the Sleeper page must move.' }] });
const askTime = call('ask_question', { id: 'time', question: 'How are run times written?', matters: 'Callers parse them.', suggested: 'ISO 8601 string', alternatives: [{ answer: 'Unix seconds', tradeoff: 'Smaller, but unreadable in logs.' }] });
const v1Script: Step[] = [
  call('read_guide', {}),
  askVersioning,
  (c) => ((refs.versioning = refIn(c)), (received.versioning = text(c)), askTime(c)),
  (c) => ((refs.time = refIn(c)), (received.time = text(c)), call('read', { path: 'packages/sleeper/src/service.ts' })(c)),
  write(() => scheduleSpecV1(refs)),
  call('submit_contract', {}),
  () => fauxAssistantMessage('Version 1 is approved.'),
];

async function defineV1(s: Open) {
  const run = s.submit('Define GET /apps/sleeper/schedule for the Sleeper page', 'r-1', true);
  const q1 = (await nextQuestion(s))[0];
  assert.deepEqual(q1.options, ['Keep the existing unversioned route', 'Add /v1 (AIP-185)', 'You decide']);
  assert.deepEqual([q1.header, q1.title, q1.detail, q1.suggested, q1.optionDescriptions], ['Versioning', 'Keep the route or add `/v1`?', 'The Sleeper page calls the route today.', 0, { 'Keep the existing unversioned route': 'No client change.', 'Add /v1 (AIP-185)': 'Room for a breaking change later, but the Sleeper page must move.' }]);
  assert.match(q1.evidence!, /Suggested: Keep the existing unversioned route[\s\S]*- Add \/v1 \(AIP-185\): Room for a breaking change/);
  assert.equal((await versions(s)).length, 0, 'nothing is sealed while a question is open');
  assert.equal(await answerQuestion(s.root, q1.id, { choice: q1.options[0], note: 'Keep it boring.' }, jf, ctx), 'ok');
  const q2 = (await nextQuestion(s, [q1.id]))[0];
  assert.equal(q2.question, 'How are run times written?');
  assert.equal(await answerQuestion(s.root, q2.id, { text: 'ISO 8601, always UTC' }, jf, ctx), 'ok');
  assert.equal(await answerQuestion(s.root, q2.id, 'You decide', jf, ctx), 'answered', 'the first answer stands');
  return { run, q1, q2 };
}

test('the question flow asks one at a time and seals version 1 only after the answers', async () => {
  const s = await open(v1Script);
  const { run, q1, q2 } = await defineV1(s);
  await approve(s);
  const done = await run;
  assert.match(received.versioning, /^Answer: Keep the existing unversioned route\. Note: Keep it boring\.\n/);
  assert.match(received.time, /^Answer in their own words: ISO 8601, always UTC\n/);
  assert.equal(done.status === 'done' && done.answer, 'Version 1 is approved.');
  const [v1] = await versions(s);
  assert.equal(v1.summary, 'Sleeper schedule endpoint, version 1');
  assert.equal(v1.outcome, 'v1');
  assert.equal((v1.verdict as { passed: boolean }).passed, true);
  const spec = JSON.parse(v1.content) as ContractSpec;
  assert.deepEqual(spec.choices.flatMap((c) => c.why.filter((w) => w.source === 'answer').map((w) => w.ref)), [q1.question, q2.question]);
  await s.close();
});

test('a json-schema spec reads its own guide, submits and passes', async () => {
  let guide = '';
  const s = await open([
    (c) => call('read_guide', { kind: 'json-schema' })(c),
    (c) => ((guide = text(c)), write(settingsSpec)(c)),
    call('submit_contract', {}),
    () => fauxAssistantMessage('Version 1 is approved.'),
  ]);
  const run = s.submit('Define the Sleeper settings file', 'r-1', true);
  await approve(s);
  const done = await run;
  assert.equal(done.status === 'done' && done.answer, 'Version 1 is approved.');
  assert.match(guide, /^# JSON Schema contract guide/);
  const [v] = await versions(s);
  assert.equal((v.verdict as { passed: boolean }).passed, true);
  assert.equal(v.outcome, 'v1 json-schema');
  assert.equal((JSON.parse(v.content) as ContractSpec).kind, 'json-schema');
  await s.close();
});

test('a spec citing a question nobody answered, or code that is not there, is sent back and never put up for approval', async () => {
  const bad = (): ContractSpec => { const v = scheduleSpecV1({ versioning: 'nope:1', time: 'nope:2' }); v.choices[2].why[1].ref = 'packages/sleeper/src/missing.ts:3'; return v; };
  let seen = '';
  const s = await open([write(bad), call('submit_contract', {}), (c) => ((seen = text(c)), fauxAssistantMessage('stopped'))], 0);
  const done = await s.submit('Define it', 'r-1', true);
  assert.equal(done.status, 'done');
  const [v] = await versions(s);
  const verdict = v.verdict as { passed: boolean; gatesFailed: string[]; checks: Array<{ notes: string[] }> };
  assert.equal(verdict.passed, false);
  assert.deepEqual(verdict.gatesFailed, ['justified.links_resolve']);
  const notes = verdict.checks.flatMap((k) => k.notes).join(' ');
  assert.match(notes, /answer ref "nope:1" does not resolve/);
  // A missing file says refs are read from the workspace root; packages/ here is a copy, not a git clone, so the hint is to clone into repo/.
  assert.match(notes, /code ref "packages\/sleeper\/src\/missing.ts:3" names no file in the workspace, which holds no clone: clone the repository into repo\/ \(git clone --depth 1 -b <branch> <url> repo\) and cite a file at packages\/sleeper\/src\/missing.ts in it as "repo\/packages\/sleeper\/src\/missing.ts:3"/);
  assert.match(seen, /cannot be put to the person for approval/);
  assert.deepEqual(await pendingApprovals(s.root, ctx), []);
  assert.equal(await sessionTitle(s), undefined, 'a failed version names nothing');
  await s.close();
});

test('end to end: version 1, then a version 2 that names the Sleeper page, each put up for approval as it passes; its recording rebuilds what the judge read', async () => {
  const bareV2 = (): ContractSpec => scheduleSpecV2(refs, []);
  const v2 = (): ContractSpec => ({ ...scheduleSpecV2(refs, [SLEEPER_PAGE]), title: 'Sleeper schedule in Unix seconds' });
  const live: Inputs[] = [];
  const builder = specInputs();
  const s = await open([
    ...v1Script,
    // Version 2: the first attempt hides the breaking change, so oasdiff's finding sends it back; the second names the caller.
    call('read', { path: 'packages/web/src/extensions/sleeper/api.ts' }),
    write(bareV2),
    call('submit_contract', {}),
    write(v2),
    call('submit_contract', {}),
    () => fauxAssistantMessage('Version 2 is approved.'),
  ], 2, async (...a) => { const got = await builder(...a); live.push(got); return got; });
  const { run } = await defineV1(s);
  const one = await approve(s);
  await run;
  assert.equal(await sessionTitle(s), 'Sleeper schedule endpoint');
  const second = s.submit('Make the run time Unix seconds', 'r-2', true);
  const two = await nextProposal(s, [one.id]);
  assert.equal(await sessionTitle(s), 'Sleeper schedule in Unix seconds', "a later version's new title renames the session");
  assert.deepEqual([two.args.version, two.args.artifact, two.args.summary], [2, 3, 'Approve GET /apps/sleeper/schedule v2'], 'the failed attempt is never put up');
  assert.ok(two.args.breaks.length > 0, 'the break against approved version 1 is computed in code for the approver');
  await resolveApproval(s.root, two.id, { decision: 'approve', by: jf }, ctx);
  const done = await second;
  assert.equal(done.status === 'done' && done.answer, 'Version 2 is approved.');
  const all = await versions(s);
  assert.equal(all.length, 3, 'v1, the rejected v2 attempt, the delivered v2');
  const failed = all[1].verdict as { passed: boolean; gatesFailed: string[]; checks: Array<{ notes: string[] }> };
  assert.deepEqual(failed.gatesFailed, ['compatible.breaking_named']);
  assert.match(failed.checks.flatMap((k) => k.notes).join(' '), /the `runs\/items\/at` response's property `type\/format` changed from `string\/date-time` to `integer`/);
  const spec = JSON.parse(all[2].content) as ContractSpec;
  assert.equal(spec.version, 2);
  assert.match(spec.compatibility!.breaking[0].callers[0], /Sleeper page/);
  assert.deepEqual((await kindOf(s, 'contract.approved')).map((r) => JSON.parse(r.content).artifact), [1, 3]);
  await s.close();
  const record = await readSession(dirname(s.workspace));
  const adapter = manifest.evalAdapters![0].judge!;
  assert.deepEqual(await Promise.all(all.map((v) => adapter.inputs!(record, v.content))), live);
  assert.deepEqual(live.map((got) => (JSON.parse(got.facts) as { breaking: unknown[] }).breaking.length > 0), [false, true, true]);
  // Had the answers come after version 1 was sealed, its answer refs could not have resolved when it was judged.
  const items = JSON.parse(JSON.stringify(record.docs['quack.questions'].items)) as Record<string, { answer: { at: number } }>;
  for (const q of Object.values(items)) q.answer.at = all[0].sealedAt + 1;
  const late = await adapter.inputs!({ ...record, docs: { ...record.docs, 'quack.questions': { items } as never } }, all[0].content);
  assert.match((JSON.parse(late.facts) as { unresolved: string[] }).unresolved.join(' '), /answer ref .* does not resolve/);
});

test('a version that passes is put up for approval in the same submit, and approving records that exact artifact', async () => {
  let seen = '';
  const s = await open([...v1Script.slice(0, -1), (c) => ((seen = text(c)), fauxAssistantMessage('Version 1 is approved.'))]);
  const { run } = await defineV1(s);
  const item = await nextProposal(s);
  assert.equal(await sessionTitle(s), 'Sleeper schedule endpoint', 'the passed spec names the session before the person is asked');
  const [v1] = await versions(s);
  assert.deepEqual([item.args.summary, item.args.version, item.args.artifact, item.args.hash], ['Approve GET /apps/sleeper/schedule v1', 1, 1, v1.hash]);
  assert.ok(Array.isArray(item.args.findings) && Array.isArray(item.args.breaks));
  assert.equal((await kindOf(s, 'contract.approved')).length, 0, 'nothing is approved before the person says so');
  assert.equal(await resolveApproval(s.root, item.id, { decision: 'approve', by: jf }, ctx), true);
  assert.equal(await resolveApproval(s.root, item.id, { decision: 'approve', by: jf }, ctx), false, 'a second tab finds it decided');
  assert.equal((await run).status, 'done');
  assert.match(seen, /passed\.[\s\S]*Version 1 is approved\./);
  const [approved] = await kindOf(s, 'contract.approved');
  assert.deepEqual([approved.outcome, approved.summary], ['v1', 'Approved version 1']);
  assert.deepEqual(JSON.parse(approved.content), { version: 1, artifact: 1, hash: v1.hash, note: 'Approved version 1', approval: item.id });
  await s.close();
});

const WITHDRAWN = /propose_contract was not run: the request was withdrawn: a person's message reached the run while it waited\. Answer the message, then call propose_contract again to ask\./;

test('a message sent while a version waits withdraws it and reaches the agent in the same turn; an answer without a change puts the same version up again', async () => {
  let turn: Msgs | undefined;
  const s = await open([...v1Script.slice(0, -1), (c) => ((turn = c), fauxAssistantMessage('ISO strings read well in logs.')), call('submit_contract', {}), () => fauxAssistantMessage('Version 1 is approved.')]);
  const { run } = await defineV1(s);
  const waiting = await nextProposal(s);
  await s.submit('Why ISO and not epoch seconds?', 'r-2', false, 'steer', [], jf);
  assert.equal(await resolveApproval(s.root, waiting.id, { decision: 'approve', by: jf }, ctx), false, 'the message withdrew it');
  // The agent answers without submitting; the run may not end there, so it puts version 1 up again.
  const again = await approve(s, [waiting.id]);
  assert.equal(again.args.artifact, 1);
  await run;
  const said = turn!.messages.map((m) => JSON.stringify(m.content)).join('\n');
  assert.match(said, WITHDRAWN);
  assert.match(said, /Why ISO and not epoch seconds\?/);
  assert.equal((await versions(s)).length, 1, 'nothing new was sealed');
  assert.deepEqual(await kindOf(s, 'contract.changes'), [], 'a withdrawn proposal records no decision');
  assert.equal((await kindOf(s, 'contract.approved')).length, 1);
  await s.close();
});

// Studio sends a follow-up while its run is busy and no proposal is shown yet; it withdraws the proposal as a steer would.
test('a follow-up queued while submit_contract waits withdraws the proposal and reaches the agent once, in the same run', async () => {
  const turns: string[] = [];
  const seen = (next: Step): Step => (c) => ((turns.push(JSON.stringify(c.messages)), next(c)));
  const s = await open([call('read_guide', { kind: 'json-schema' }), write(settingsSpec), call('submit_contract', {}), seen(call('submit_contract', {})), seen(() => fauxAssistantMessage('ok')), seen(() => fauxAssistantMessage('ok'))]);
  const run = s.submit('Define the Sleeper settings file', 'r-1');
  const waiting = await nextProposal(s);
  const queued = await s.submit('Also cover the timezone field', 'r-2', false, 'followUp', [], jf);
  assert.equal(queued.status, 'queued');
  assert.equal(await resolveApproval(s.root, waiting.id, { decision: 'approve', by: jf }, ctx), false, 'the follow-up withdrew it');
  await approve(s, [waiting.id]);
  await run;
  assert.equal((await (await s.harness.submission(queued.submissionId as never, ctx))!.wait(ctx)).status, 'done');
  const took = turns.filter((t) => t.includes('Also cover the timezone field'));
  assert.ok(took.length && took.every((t) => t.split('Also cover the timezone field').length === 2), 'the follow-up reached the model, once');
  assert.match(took[0]!, WITHDRAWN);
  assert.deepEqual(await s.queued(), []);
  await s.close();
});

test('after a rejection, an agent that keeps answering without putting the version up again is asked once, then the run ends', async () => {
  const asked: string[] = [];
  const reply = (c: Msgs) => ((asked.push(text(c)), fauxAssistantMessage('I would rather discuss it first.')));
  const s = await open([call('read_guide', { kind: 'json-schema' }), write(settingsSpec), call('submit_contract', {}), ...Array.from({ length: 6 }, () => reply)]);
  const run = s.submit('Define the Sleeper settings file', 'r-1', true);
  await resolveApproval(s.root, (await nextProposal(s)).id, { decision: 'reject', reason: 'Why a string?', by: jf }, ctx);
  const done = await run;
  assert.equal(done.status === 'done' && done.answer, 'I would rather discuss it first.');
  assert.equal(asked.length, 2, `one answer to the rejection, one to the reminder: ${JSON.stringify(asked)}`);
  assert.match(asked[1], /passed its checks but is not approved/);
  await s.close();
});

test('a decision whose record was lost in a crash is written once when the proposal next runs', async () => {
  let seen = '';
  const s = await open([...v1Script.slice(0, -1), () => fauxAssistantMessage('Approved.'), call('propose_contract', {}), (c) => ((seen = text(c)), fauxAssistantMessage('Done.'))]);
  const { run } = await defineV1(s);
  const one = await approve(s);
  await run;
  // The crash window: the verdict is stored, the record is not.
  await s.root.commit(async (tx) => { (await tx.doc(Artifacts, s.root.id)).kinds['contract.approved'].versions.length = 0; }, ctx);
  assert.equal((await kindOf(s, 'contract.approved')).length, 0);
  await s.submit('Propose it again', 'r-2', true);
  const recovered = await kindOf(s, 'contract.approved');
  assert.equal(recovered.length, 1, 'the lost approval is written once');
  assert.equal(JSON.parse(recovered[0].content).approval, one.id);
  assert.match(recovered[0].hash, /^[0-9a-f]{64}$/);
  assert.match(seen, /Version 1 is approved\./);
  assert.equal((await pendingApprovals(s.root, ctx)).length, 0, 'an approved version is not asked again');
  await s.close();
});

test('propose_contract refuses when no spec has passed', async () => {
  let seen = '';
  const s = await open([call('propose_contract', {}), (c) => ((seen = text(c)), fauxAssistantMessage('ok'))]);
  await s.submit('Define it', 'r-1', true);
  assert.match(seen, /Only the newest sealed version can be put up for approval/);
  await s.close();
});

const retitled = () => ({ ...scheduleSpecV1(refs), title: 'Sleeper schedule, retitled' });

test('a change request after a pass gets a fresh check budget; a failed newest version is never put up, and no older pass stands in', async () => {
  let checked = '', proposed = '';
  const s = await open([...v1Script.slice(0, -1), write(retitled), call('submit_contract', {}), (c) => ((checked = text(c)), call('propose_contract', {})(c)), (c) => ((proposed = text(c)), fauxAssistantMessage('ok'))], 0);
  const { run } = await defineV1(s);
  await resolveApproval(s.root, (await nextProposal(s)).id, { decision: 'reject', reason: 'Retitle it', by: jf }, ctx);
  await run;
  assert.doesNotMatch(checked, /the most it gets/);
  const [, v2] = await versions(s);
  assert.equal((v2.verdict as { passed: boolean }).passed, false, 'the revision is checked, and fails');
  assert.match(proposed, /Only the newest sealed version can be put up/);
  assert.equal((await pendingApprovals(s.root, ctx)).length, 0);
  assert.equal(await sessionTitle(s), 'Sleeper schedule endpoint', 'the failed retitle keeps the passed name');
  await s.close();
});

test('a later run gets a fresh revision limit, and a version released at the limit is not counted as delivered', async () => {
  const attempt = (n: number) => [write(() => ({ ...scheduleSpecV1({ versioning: 'nope:1', time: 'nope:2' }), title: `Attempt ${n}` })), call('submit_contract', {})];
  const s = await open([...attempt(1), ...attempt(2), () => fauxAssistantMessage('stopped'), ...attempt(3), ...attempt(4), () => fauxAssistantMessage('stopped')], 1);
  await s.submit('Define it', 'r-1', true);
  await s.submit('Try again', 'r-2', true);
  const verdicts = (await versions(s)).map((v) => v.verdict as { limitReached: boolean; gatesFailed: string[] });
  assert.deepEqual(verdicts.map((v) => v.limitReached), [false, true, false, true]);
  assert.deepEqual(verdicts[2].gatesFailed, ['justified.links_resolve'], 'still spec version 1');
  await s.close();
});

// #2374: artifact 1 waits, a deploy makes the rerun of submit_contract fail before its gate, and the agent seals artifact 2.
test('a proposal whose rerun fails before its gate is withdrawn, and the newer artifact is approved over it', async () => {
  const s = await open([call('read_guide', { kind: 'json-schema' }), write(settingsSpec), call('submit_contract', {})]);
  void s.submit('Define the Sleeper settings file', 'r-1');
  const stale = await nextProposal(s);
  await s.close();
  await writeFile(join(s.workspace, 'artifacts/spec.json'), JSON.stringify({ ...settingsSpec(), baseline: 42 }));

  let told = '', again = '';
  // A passed version counts as delivered for the judge's version check, so the replacement is spec version 2.
  const retitled = () => ({ ...settingsSpec(), title: 'Sleeper settings, retitled', version: 2 });
  const steps: Step[] = [(c) => ((told = text(c)), write(retitled)(c)), call('submit_contract', {}), () => fauxAssistantMessage('Version 2 is approved.'), call('propose_contract', {}), (c) => ((again = text(c)), fauxAssistantMessage('Done.'))];
  const s2 = await open(steps, 2, undefined, dirname(s.workspace));
  const run = s2.submit('Define the Sleeper settings file', 'r-1', true);
  const fresh = await nextProposal(s2, [stale.id]);
  assert.match(told, /does not match contract\.spec/, 'the rerun failed before the gate');
  assert.deepEqual([stale.args.artifact, fresh.args.artifact, fresh.args.version], [1, 2, 2]);
  assert.deepEqual((await pendingApprovals(s2.root, ctx)).map((a) => a.id), [fresh.id], 'no pending ask for the replaced version');
  assert.equal(await resolveApproval(s2.root, stale.id, { decision: 'approve', by: jf }, ctx), false, 'approving the withdrawn ask is refused');
  assert.equal(await resolveApproval(s2.root, fresh.id, { decision: 'approve', by: jf }, ctx), true);
  const done = await run;
  assert.equal(done.status === 'done' && done.answer, 'Version 2 is approved.');
  const row = (await s2.harness.snapshot(Approvals, s2.root.id, ctx))!.items[stale.id]!;
  assert.ok(row.withdrawn && ['task_ended', 'superseded'].includes(row.withdrawn.reason), JSON.stringify(row.withdrawn));

  // A person's approve that landed just before the withdrawal stays on the row as a record; the next proposal neither records nor counts it.
  await s2.root.commit(async (tx) => { (await tx.doc(Approvals, s2.root.id)).items[stale.id]!.verdict = { decision: 'approve', reason: null, by: jf, at: 1 }; }, ctx);
  await s2.submit('Propose it again', 'r-2', true);
  assert.match(again, /Version 2 is approved\./, 'the base is artifact 2, not the artifact 1 the late verdict names');
  assert.deepEqual((await kindOf(s2, 'contract.approved')).map((r) => JSON.parse(r.content).artifact), [2], 'the verdict on artifact 1 is never recorded');
  await s2.close();
});

test('a review in the rejection reaches the agent with how to answer it, and reply_comment answers a comment', async () => {
  const said: string[] = [];
  const v2 = () => ({ ...settingsSpec(), version: 2 });
  const s = await open([
    call('read_guide', { kind: 'json-schema' }), write(settingsSpec), call('submit_contract', {}),
    (c) => ((said.push(text(c)), call('reply_comment', { id: 'cmt_1', text: 'Kept closed: a typo must fail loudly.' })(c))),
    (c) => ((said.push(text(c)), write(v2)(c))), call('submit_contract', {}),
    () => fauxAssistantMessage('Version 2 is approved.'),
  ]);
  const run = s.submit('Define the Sleeper settings file', 'r-1', true);
  const reason = reviewReason('One thing.', { review: 'rev_1', artifact: 1, comments: [{ id: 'cmt_1', anchor: { on: 'choice', id: 'closed' }, body: 'Why closed?' }] });
  await resolveApproval(s.root, (await nextProposal(s)).id, { decision: 'reject', reason, by: jf }, ctx);
  await approve(s);
  await run;
  assert.match(said[0]!, /ends in a ```quack-review block of review comments: for each comment, name its id in the addresses/);
  assert.deepEqual(lastReviewBlock(said[0]!.replace(/^Rejected by [^:]*: /, ''))?.block.comments.map((c) => c.id), ['cmt_1'], 'the agent reads the block whole');
  assert.match(said[1]!, /Replied to comment cmt_1\./);
  assert.equal(((await versions(s))[1]!.verdict as { passed: boolean }).passed, true, 'no gate counts the comments: the Contracts app decides what is addressed');
  assert.deepEqual((await kindOf(s, 'contract.reply')).map((r) => JSON.parse(r.content)), [{ comment: 'cmt_1', text: 'Kept closed: a typo must fail loudly.' }]);
  assert.deepEqual((await kindOf(s, 'contract.changes')).map((r) => r.summary), ['One thing. · 1 review comment']);
  await s.close();
});

test('reply_comment refuses a 21st reply to one comment with an error the agent reads', async () => {
  const replies = Array.from({ length: 21 }, (_, i) => call('reply_comment', { id: 'cmt_1', text: `reply ${i}` }));
  let last = '';
  const s = await open([...replies, (c) => ((last = text(c)), fauxAssistantMessage('done'))]);
  await s.submit('Reply a lot', 'r-1', true);
  assert.equal((await kindOf(s, 'contract.reply')).length, 20);
  assert.match(last, /already has 20 replies/);
  await s.close();
});

test('reject-and-discard on an unwanted proposal leaves no pending ask, no re-proposal, and restores the approved version', async () => {
  const said: string[] = [];
  const v2 = () => ({ ...settingsSpec(), version: 2, baseline: 1, compatibility: { breaking: [], safe: ['minor tweak'] }, changes: [{ choice: 'closed', was: 'strict', now: 'strict with note', why: 'minor tweak' }] });
  const s = await open([
    call('read_guide', { kind: 'json-schema' }), write(settingsSpec), call('submit_contract', {}),
    () => fauxAssistantMessage('Version 1 is approved.'),
    write(v2), call('submit_contract', {}),
    (c) => ((said.push(text(c)), call('discard_draft', {})(c))),
    (c) => ((said.push(text(c)), fauxAssistantMessage('Dropped v2; version 1 stands.'))),
  ]);
  const first = s.submit('Define settings', 'r-1', true);
  const p1 = await approve(s);
  assert.equal((await first).status, 'done');

  const second = s.submit('Minor tweak', 'r-2', true);
  const p2 = await nextProposal(s, [p1.id]);
  await resolveApproval(s.root, p2.id, { decision: 'reject', reason: 'not wanted, version 1 stands, drop this version', by: jf }, ctx);
  const res = await second;
  assert.equal(res.status === 'done' && res.answer, 'Dropped v2; version 1 stands.');
  assert.match(said[0]!, /call discard_draft/);
  assert.match(said[1]!, /version 1 stands as the current version/);
  const approvals = (await s.harness.snapshot(Approvals, s.root.id, ctx))!;
  assert.deepEqual(await pendingApprovals(s.root, ctx), [], 'no pending ask remains');
  assert.equal(Object.keys(approvals.items).length, 2, 'no re-proposal was created');
  const onDisk = JSON.parse(await readFile(join(s.workspace, 'artifacts/spec.json'), 'utf8')) as ContractSpec;
  assert.equal(onDisk.version, 1, 'workspace draft restored to approved version 1');
  const allArtifacts = Object.values((await s.harness.snapshot(Artifacts, s.root.id, ctx))?.kinds ?? {}).flatMap((k) => k?.versions ?? []);
  const rec = contractRecordOf('contracts:test', undefined, allArtifacts, []);
  assert.deepEqual([rec.draft, rec.approved, rec.pendingApproval], [1, 1, false]);
  await s.close();
});

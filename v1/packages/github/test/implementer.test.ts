import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts, pendingApprovals, resolveApproval } from '@quack/sdk';
import { openSession } from '@quack/session';
import { githubSessionExtensions } from '../src/index.ts';
import { claimedCommands } from '../src/claims.ts';
import { cleanup, fakeService, remotes, sh, tmp } from './fixtures.ts';

after(cleanup);

const call = (name: string, args: Record<string, unknown> = {}) => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });
const checkout = () => call('checkout_task', { repository: 'acme/widgets', number: 7 });
const PR_MD = '# fix(app): bump a to 3\n\nWhat: a is 3.\nWhy: the issue asked.\nVerified: node app.ts.\n';

async function implement(responses: (ctx: { repo: () => string; seen: string[]; live: Record<string, unknown>; bare: string }) => FauxResponseStep[], pr: Record<string, unknown> = {}) {
  const { base, head } = await remotes();
  const live: Record<string, unknown> = { headSha: head, ...pr };
  const service = await fakeService(head, live);
  const seen: string[] = [];
  let workspace = '';
  const bare = join(fileURLToPath(base), 'acme/widgets.git');
  const faux = fauxProvider({ models: [{ id: 'worker' }] });
  faux.setResponses(responses({ repo: () => join(workspace, 'repo'), seen, live, bare }));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const extensions = [CodingTools, ...githubSessionExtensions({ role: 'implementer', serviceUrl: service.url, serviceToken: 'svc', gitBaseUrl: base })];
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  workspace = session.workspace;
  return { session, head, bare, seen, service };
}

const record = (seen: string[], next: () => ReturnType<typeof fauxAssistantMessage>) => (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), next());

// Commits a change on the checked-out branch the way the agent would, and writes the PR text.
async function change(repo: string, subject = 'fix(app): bump a to 3') {
  await writeFile(join(repo, 'app.ts'), 'export const a = 3;\n');
  sh(repo, 'commit', '-qam', subject);
  await mkdir(join(repo, '..', 'artifacts'), { recursive: true });
  await writeFile(join(repo, '..', 'artifacts', 'pr.md'), PR_MD);
}

// The gate is durable; the test waits for the approval row, then answers it as an operator would.
async function answer(session: Awaited<ReturnType<typeof openSession>>, decision: 'approve' | 'reject') {
  const deadline = Date.now() + 15_000;
  for (;;) {
    const [pending] = await pendingApprovals(session.root, BACKGROUND_CONTEXT);
    if (pending) {
      await resolveApproval(session.root, pending.id, { decision, by: { kind: 'person', name: 'operator' }, reason: decision === 'reject' ? 'not now' : undefined }, BACKGROUND_CONTEXT);
      return pending;
    }
    if (Date.now() > deadline) throw new Error('no approval was requested');
    await new Promise((r) => setTimeout(r, 10));
  }
}

test('checkout_task puts the PR head branch in ./repo, not detached, with the facts the task needs', async () => {
  const { session, head, seen } = await implement(({ seen }) => [checkout(), record(seen, () => fauxAssistantMessage('ok'))]);
  await session.submit('Fix acme/widgets#7', 'fx-1', true);
  const repo = join(session.workspace, 'repo');
  assert.equal(sh(repo, 'rev-parse', '--abbrev-ref', 'HEAD'), 'bump');
  assert.equal(sh(repo, 'rev-parse', 'HEAD'), head);
  assert.match(seen[0], /Checked out branch bump at /);
  assert.match(seen[0], /CI checks: ci=success/);
  assert.match(seen[0], /Linked issue #3: a should be 2/);
  assert.match(seen[0], /Comments stay at two lines/);
  await session.close();
});

test('checkout_task shows open review feedback from anyone, bounded, and says what it cut', async () => {
  const feedback = {
    reviews: [{ author: 'maria', state: 'CHANGES_REQUESTED', commit: 'a'.repeat(40), current: false, body: 'Please handle the empty list.' }],
    comments: [{ author: 'maria', path: 'app.ts', line: 3, body: 'This loop is off by one.' }, ...Array.from({ length: 10 }, (_, i) => ({ author: 'bot', path: `f${i}.ts`, line: i + 1, body: 'x'.repeat(1_400) }))],
  };
  const { session, seen } = await implement(({ seen }) => [checkout(), record(seen, () => fauxAssistantMessage('ok'))], { feedback });
  await session.submit('Fix acme/widgets#7', 'fx-1', true);
  assert.match(seen[0], /maria CHANGES_REQUESTED on aaaaaaa \(an older head\):\\nPlease handle the empty list\./);
  assert.match(seen[0], /maria on app\.ts:3:\\nThis loop is off by one\./);
  assert.match(seen[0], /\d+ more review items cut to fit the size budget/);
  await session.close();
});

test('a PR whose head is in a fork is refused with a plain message and nothing is cloned', async () => {
  const { session, seen } = await implement(({ seen }) => [checkout(), record(seen, () => fauxAssistantMessage('ok'))], { headRepo: 'stranger/widgets' });
  await session.submit('Fix acme/widgets#7', 'fx-1', true);
  assert.match(seen[0], /Nothing checked out: the head branch lives in stranger\/widgets, a fork; the GitHub App cannot push there/);
  assert.equal(existsSync(join(session.workspace, 'repo')), false);
  await session.close();
});

test('deliver names every failed code check: no commit, no pr.md, then a bad subject, and a dirty tree', async () => {
  const { session, seen } = await implement(({ repo, seen }) => [
    checkout(),
    record(seen, () => call('deliver')),
    async (c) => {
      seen.push(JSON.stringify(c.messages.at(-1)));
      await writeFile(join(repo(), 'app.ts'), 'export const a = 3;\n');
      sh(repo(), 'commit', '-qam', 'updated stuff');
      await writeFile(join(repo(), 'new.ts'), 'x\n');
      return call('deliver');
    },
    record(seen, () => fauxAssistantMessage('stop')),
  ]);
  await session.submit('Fix acme/widgets#7', 'fx-1', true);
  assert.match(seen[1], /there is no new commit since checkout/);
  assert.match(seen[1], /artifacts\/pr\.md cannot be read; write it first/);
  assert.match(seen[2], /commit subject is not Conventional Commits .*: updated stuff/);
  assert.match(seen[2], /the working tree has uncommitted changes/);
  assert.deepEqual(await pendingApprovals(session.root, BACKGROUND_CONTEXT), [], 'a failed check never reaches the gate');
  await session.close();
});

test('an approved deliver pushes the commits to the PR branch; the approver was shown the text, commits and diffstat', async () => {
  const { session, bare, seen } = await implement(({ repo, seen }) => [checkout(), async () => (await change(repo()), call('deliver')), record(seen, () => fauxAssistantMessage('Delivered.'))]);
  const run = session.submit('Fix acme/widgets#7', 'fx-1', true);
  const pending = await answer(session, 'approve');
  assert.equal(pending.tool, 'deliver');
  const shown = pending.args as Record<string, string>;
  assert.equal(shown.body, PR_MD);
  assert.match(shown.commits, /fix\(app\): bump a to 3/);
  assert.match(shown.diffStat, /app\.ts \| 2 \+-/);
  assert.equal((await run).status, 'done');
  const pushed = sh(join(session.workspace, 'repo'), 'rev-parse', 'HEAD');
  assert.equal(sh(bare, 'rev-parse', 'refs/heads/bump'), pushed);
  assert.match(seen[0], /Pushed [0-9a-f]{40} to bump: https:\/\/github.com\/acme\/widgets\/pull\/7/);
  const sealed = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))!.kinds['github.pr_body'].versions[0];
  assert.equal(sealed.summary, 'fix(app): bump a to 3');
  assert.deepEqual(sealed.links, [{ label: 'Pushed', url: 'https://github.com/acme/widgets/pull/7' }]);
  await session.close();
});

test("an approved deliver sets the PR's title and body to the pr.md the approver saw, keeping the issues the live body closed", async () => {
  const { session, service } = await implement(({ repo, seen }) => [checkout(), async () => (await change(repo()), call('deliver')), record(seen, () => fauxAssistantMessage('Delivered.'))], { body: 'Refactors the loop. This fixes #3, and closes #4.' });
  const run = session.submit('Fix acme/widgets#7', 'fx-edit', true);
  await answer(session, 'approve');
  assert.equal((await run).status, 'done');
  assert.deepEqual(service.edited, [{ path: '/pulls/acme/widgets/7', title: 'fix(app): bump a to 3', body: 'What: a is 3.\nWhy: the issue asked.\nVerified: node app.ts.\n\nCloses #3\n\nCloses #4' }]);
  await session.close();
});

test('a deliver whose pr.md already closes the issue adds no second closing line', async () => {
  const { session, service } = await implement(({ repo, seen }) => [checkout(), async () => (await change(repo()), await writeFile(join(repo(), '..', 'artifacts', 'pr.md'), `${PR_MD}\nFixes #3.\n`), call('deliver')), record(seen, () => fauxAssistantMessage('Delivered.'))], { body: 'Fixes #3' });
  const run = session.submit('Fix acme/widgets#7', 'fx-edit2', true);
  await answer(session, 'approve');
  assert.equal((await run).status, 'done');
  assert.equal(service.edited[0].body, 'What: a is 3.\nWhy: the issue asked.\nVerified: node app.ts.\n\nFixes #3.');
  await session.close();
});

test('a rejected deliver pushes nothing', async () => {
  const { session, bare, seen } = await implement(({ repo, seen }) => [checkout(), async () => (await change(repo()), call('deliver')), record(seen, () => fauxAssistantMessage('Stopped.'))]);
  const run = session.submit('Fix acme/widgets#7', 'fx-1', true);
  await answer(session, 'reject');
  await run;
  assert.match(seen[0], /Rejected by operator: not now/);
  assert.equal(sh(bare, 'branch', '--list', 'bump'), '');
  await session.close();
});

test('an approved deliver refuses when the PR branch moved since checkout', async () => {
  const { session, bare, seen } = await implement(({ repo, seen, live }) => [
    checkout(),
    async () => { await change(repo()); live.headSha = 'f'.repeat(40); return call('deliver'); },
    record(seen, () => fauxAssistantMessage('Stopped.')),
  ]);
  const run = session.submit('Fix acme/widgets#7', 'fx-1', true);
  await answer(session, 'approve');
  await run;
  assert.match(seen[0], /Nothing was pushed: bump moved from [0-9a-f]{40} to f{40} since checkout/);
  assert.equal(sh(bare, 'branch', '--list', 'bump'), '');
  await session.close();
});

// Delivers a pr.md whose "How verified" claims `claim` (plus `extra`) after the session ran the `ran` commands.
async function claiming(claim: string, ran: string[], extra = '') {
  const text = PR_MD.replace('Verified: node app.ts.', `How verified: \`${claim}\` passed.${extra}`);
  const { session, seen } = await implement(({ repo, seen }) => [
    checkout(),
    ...ran.map((command) => call('bash', { command })),
    async () => { await change(repo()); await writeFile(join(repo(), '..', 'artifacts', 'pr.md'), text); return call('deliver'); },
    record(seen, () => fauxAssistantMessage('stop')),
  ]);
  return { session, seen, run: session.submit('Fix acme/widgets#7', 'fx-1', true) };
}

test('a claimed command the session ran, behind a cd and different quoting, reaches approval', async () => {
  const { session, run, seen } = await claiming('echo "ok"', ['cd repo && echo ok']);
  await answer(session, 'approve');
  assert.equal((await run).status, 'done');
  assert.doesNotMatch(seen.at(-1)!, /Not delivered/);
  await session.close();
});

test('a claimed command the session never ran fails deliver, naming it', async () => {
  const { session, run, seen } = await claiming('npm test', ['echo hi']);
  await run;
  assert.match(seen.at(-1)!, /Not delivered/);
  assert.match(seen.at(-1)!, /artifacts\/pr\.md says you ran `npm test`, but this session never ran it/);
  assert.deepEqual(await pendingApprovals(session.root, BACKGROUND_CONTEXT), []);
  await session.close();
});

test('a claimed pass whose only run exited non-zero fails deliver', async () => {
  const { session, run, seen } = await claiming('ls nonexistent-dir', ['ls nonexistent-dir']);
  await run;
  assert.match(seen.at(-1)!, /`ls nonexistent-dir` passed, but every run of it exited non-zero/);
  await session.close();
});

test('a claim whose line says it failed is backed by a failed run', async () => {
  const { session, run, seen } = await claiming('ls nonexistent-dir', ['ls nonexistent-dir'], ' It failed as expected.');
  await answer(session, 'approve');
  assert.equal((await run).status, 'done');
  assert.doesNotMatch(seen.at(-1)!, /Not delivered/);
  await session.close();
});

test('a failure word must be a word: "redo" does not excuse a claimed pass whose runs all failed', async () => {
  const { session, run, seen } = await claiming('ls nonexistent-dir', ['ls nonexistent-dir'], ' After a redo of the codegen step.');
  await run;
  assert.match(seen.at(-1)!, /`ls nonexistent-dir` passed, but every run of it exited non-zero/);
  await session.close();
});

test('a claim on the last line keeps its last character when pr.md ends without a newline', () => {
  assert.deepEqual(claimedCommands('# t\n\nHow verified: `npm test` broke').map((c) => c.line), ['How verified: `npm test` broke']);
});

test('a backticked commit id outside the commits made since checkout fails deliver', async () => {
  const { session, run, seen } = await claiming('echo ok', ['echo ok'], ' Fixed in `deadbeef1`.');
  await run;
  assert.match(seen.at(-1)!, /names commit deadbeef1, which is not among the commits made since checkout/);
  await session.close();
});

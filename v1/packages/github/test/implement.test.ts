import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts, pendingApprovals, resolveApproval } from '@quack/sdk';
import { openSession } from '@quack/session';
import { githubSessionExtensions, triggerFor } from '../src/index.ts';
import { cleanup, fakeService, remotes, sh, tmp } from './fixtures.ts';

after(cleanup);

const repository = { name: 'widgets', owner: { login: 'acme' } };
const issueComment = (body: string, association = 'MEMBER', pull = false) => ({
  action: 'created', repository, comment: { body, author_association: association, user: { login: 'someone' } },
  issue: { number: 12, title: 'Add cache TTL', body: 'Entries never expire.', labels: [{ name: 'bug' }, { name: 'cache' }], ...(pull ? { pull_request: {} } : {}) },
});
const implement = (body: string, association?: string, pull?: boolean) => triggerFor('issue_comment', issueComment(body, association, pull), 'quack:review', 'quack-app');

test('a trusted /implement on an issue starts the implementer with the issue facts and the instruction', () => {
  const t = implement('/implement keep it under 20 lines')!;
  assert.equal(t.role, 'implementer');
  assert.equal(t.sessionKey, 'github:acme/widgets#12');
  assert.equal(t.issue, true);
  assert.equal(t.followUp, undefined);
  assert.match(t.input, /Implement issue acme\/widgets#12 \(\/implement by someone\)/);
  assert.match(t.input, /Title: Add cache TTL\nLabels: bug, cache\n\nEntries never expire\./);
  assert.match(t.input, /Instruction: keep it under 20 lines/);
  assert.match(implement(' /implement ')!.input, /Instruction: \(none; implement the issue as written\)/);
});

test('/implement tells the implementer to start with checkout_task', () => {
  assert.match(implement('/implement')!.input, /Start with checkout_task: .*open review feedback/);
});

test('/implement from an untrusted author, on a pull request, or glued to other text starts nothing', () => {
  assert.equal(implement('/implement', 'CONTRIBUTOR'), undefined);
  assert.equal(implement('/implement', 'MEMBER', true), undefined);
  assert.equal(implement('/implementation'), undefined);
  assert.equal(implement('please /implement'), undefined);
});

const ISSUE = { owner: 'acme', repo: 'widgets', number: 12, title: 'Add cache TTL!', body: 'Entries never expire.', author: 'dev', state: 'open', labels: ['bug'], baseRef: 'main' };
const BRANCH = 'quack/12-add-cache-ttl';
const PR_MD = '# feat(app): bump a to 3\n\nWhat: a is 3.\nWhy: the issue asked.\nVerified: node app.ts.\n';
const call = (name: string, args: Record<string, unknown> = {}) => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });

async function work(responses: (ctx: { repo: () => string; seen: string[] }) => FauxResponseStep[], issue: Record<string, unknown> = ISSUE) {
  const { base, head } = await remotes();
  const service = await fakeService(head, {}, issue);
  const seen: string[] = [];
  let workspace = '';
  const bare = join(fileURLToPath(base), 'acme/widgets.git');
  const faux = fauxProvider({ models: [{ id: 'worker' }] });
  faux.setResponses(responses({ repo: () => join(workspace, 'repo'), seen }));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const extensions = [CodingTools, ...githubSessionExtensions({ role: 'implementer', serviceUrl: service.url, serviceToken: 'svc', gitBaseUrl: base })];
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  workspace = session.workspace;
  return { session, service, bare, head, seen };
}

const record = (seen: string[], next: () => ReturnType<typeof fauxAssistantMessage>) => (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), next());
const checkout = () => call('checkout_task', { repository: 'acme/widgets', number: 12 });

async function commit(repo: string) {
  await writeFile(join(repo, 'app.ts'), 'export const a = 3;\n');
  sh(repo, 'commit', '-qam', 'feat(app): bump a to 3');
  await mkdir(join(repo, '..', 'artifacts'), { recursive: true });
  await writeFile(join(repo, '..', 'artifacts', 'pr.md'), PR_MD);
}

async function answer(session: Awaited<ReturnType<typeof openSession>>, decision: 'approve' | 'reject') {
  const deadline = Date.now() + 15_000;
  for (;;) {
    const [pending] = await pendingApprovals(session.root, BACKGROUND_CONTEXT);
    if (pending) {
      await resolveApproval(session.root, pending.id, { decision, by: 'operator', reason: decision === 'reject' ? 'not now' : undefined }, BACKGROUND_CONTEXT);
      return pending;
    }
    if (Date.now() > deadline) throw new Error('no approval was requested');
    await new Promise((r) => setTimeout(r, 10));
  }
}

test('checkout_task for an issue cuts quack/<n>-<slug> from the default branch and returns the issue and conventions', async () => {
  const { session, seen } = await work(({ seen }) => [checkout(), record(seen, () => fauxAssistantMessage('ok'))]);
  await session.submit('Implement acme/widgets#12', 'im-1', true);
  const repo = join(session.workspace, 'repo');
  assert.equal(sh(repo, 'rev-parse', '--abbrev-ref', 'HEAD'), BRANCH);
  assert.equal(sh(repo, 'rev-parse', 'HEAD'), sh(repo, 'rev-parse', 'origin/main'));
  assert.match(seen[0], new RegExp(`Created branch ${BRANCH} from main at [0-9a-f]{40}`));
  assert.match(seen[0], /Labels: bug\\nIssue:\\nEntries never expire\./);
  assert.match(seen[0], /Comments stay at two lines/);
  await session.close();
});

test('an approved deliver pushes the branch and opens a pull request that closes the issue', async () => {
  const { session, service, bare, seen } = await work(({ repo, seen }) => [checkout(), async () => (await commit(repo()), call('deliver')), record(seen, () => fauxAssistantMessage('Delivered.'))]);
  const run = session.submit('Implement acme/widgets#12', 'im-1', true);
  const pending = await answer(session, 'approve');
  assert.equal((pending.args as Record<string, string>).summary, 'Open a PR from quack/12-add-cache-ttl into main for #12: feat(app): bump a to 3');
  assert.equal((await run).status, 'done');
  assert.equal(sh(bare, 'rev-parse', `refs/heads/${BRANCH}`), sh(join(session.workspace, 'repo'), 'rev-parse', 'HEAD'));
  assert.deepEqual(service.opened, [{ owner: 'acme', repo: 'widgets', head: BRANCH, base: 'main', title: 'feat(app): bump a to 3', body: 'What: a is 3.\nWhy: the issue asked.\nVerified: node app.ts.\n\nCloses #12' }]);
  assert.match(seen[0], /opened https:\/\/github.com\/acme\/widgets\/pull\/13/);
  const sealed = (await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT))!.kinds['github.pr_body'].versions[0];
  assert.deepEqual(sealed.links, [{ label: 'Opened', url: 'https://github.com/acme/widgets/pull/13' }]);
  await session.close();
});

test('a rejected deliver pushes and opens nothing', async () => {
  const { session, service, bare, seen } = await work(({ repo, seen }) => [checkout(), async () => (await commit(repo()), call('deliver')), record(seen, () => fauxAssistantMessage('Stopped.'))]);
  const run = session.submit('Implement acme/widgets#12', 'im-1', true);
  await answer(session, 'reject');
  await run;
  assert.match(seen[0], /Rejected by operator: not now/);
  assert.equal(sh(bare, 'branch', '--list', BRANCH), '');
  assert.deepEqual(service.opened, []);
  await session.close();
});

test('a branch that already exists remotely with other commits is refused before any approval', async () => {
  const { session, service, bare, head, seen } = await work(({ repo, seen }) => [
    checkout(),
    async () => (await commit(repo()), sh(bare, 'branch', BRANCH, head), call('deliver')),
    record(seen, () => fauxAssistantMessage('Stopped.')),
  ]);
  await session.submit('Implement acme/widgets#12', 'im-1', true);
  assert.match(seen[0], /Not delivered/);
  assert.match(seen[0], /the branch quack\/12-add-cache-ttl already exists on acme\/widgets with other commits/);
  assert.deepEqual(await pendingApprovals(session.root, BACKGROUND_CONTEXT), []);
  assert.equal(sh(bare, 'rev-parse', `refs/heads/${BRANCH}`), head);
  assert.deepEqual(service.opened, []);
  await session.close();
});

test('a closed issue is not checked out', async () => {
  const { session, seen } = await work(({ seen }) => [checkout(), record(seen, () => fauxAssistantMessage('ok'))], { ...ISSUE, state: 'closed' });
  await session.submit('Implement acme/widgets#12', 'im-1', true);
  assert.match(seen[0], /the issue is closed; nothing to implement/);
  await session.close();
});

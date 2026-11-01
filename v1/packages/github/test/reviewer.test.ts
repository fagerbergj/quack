import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts } from '@quack/sdk';
import { buildModels, openSession, parseDeployment } from '@quack/session';
import { githubSessionExtensions } from '../src/index.ts';
import { cleanup, fakeService, listen, remotes, sh, tmp } from './fixtures.ts';

after(cleanup);

const PITFALLS = /toolchain_as_defect|contradicted|no_reason_given|preference_only|blocks_without_consequence|wrong_output_below_blocking/;
const clefUnits: string[] = [];
async function fakeClef() {
  return `${await listen(
    createServer(async (req, res) => {
      let raw = '';
      for await (const c of req) raw += c;
      const { state, questions } = JSON.parse(raw);
      clefUnits.push(Object.keys(state).join('+'));
      const answers = Object.fromEntries(Object.keys(questions).map((q) => [q, { type: 'noul', noul: PITFALLS.test(q) ? 0.04 : 0.96 }]));
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ answers, usage: { input_tokens: 300, output_tokens: 0 } }));
    }),
  )}/v1`;
}

const review = `## Takeaway
The change does what issue #3 asks; approve.
## Verdict
approve
## Findings
- nit: app.ts:1 — a named constant would say why the value is 2.
## Verified
- read app.ts:1 on the PR head: \`export const a = 2;\`
`;

async function reviewRun(responses: (seen: string[], advance: () => string) => FauxResponseStep[]) {
  process.env.QUACK_CLEF_KEY = 'none';
  const { base, head } = await remotes();
  const live = { headSha: head };
  const service = await fakeService(head, live);
  // Pushes a second commit to the PR (a file the review does not touch) and makes it the live head.
  const advance = () => {
    const work = join(fileURLToPath(base), 'acme/widgets.git');
    const next = sh(work, 'commit-tree', '-p', head, '-m', 'later push', `${head}^{tree}`);
    sh(work, 'update-ref', 'refs/pull/7/head', next);
    return (live.headSha = next);
  };
  const clefUrl = await fakeClef();
  const seen: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'worker' }] });
  faux.setResponses(responses(seen, advance));
  const models = buildModels(parseDeployment({ providers: { clef: { baseUrl: clefUrl, api: 'systemone', apiKeyEnv: 'QUACK_CLEF_KEY', models: { clef: { contextWindow: 8192 } } } } }));
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const extensions = [CodingTools, ...githubSessionExtensions({ serviceUrl: service.url, serviceToken: 'svc', gitBaseUrl: base, classifier: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } })];
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  const done = await session.submit('Review pull request acme/widgets#7', 'gh-1', true);
  return { done, seen, service, head, session };
}

const call = (name: string, args: Record<string, unknown>) => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });
const recorder = (seen: string[]) => (msg: ReturnType<typeof fauxAssistantMessage>) => (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), msg);

test('a reviewer run checks out the PR, has its review judged, and posts exactly the checked version', async () => {
  const { done, seen, service, head, session } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [
      call('checkout_pr', { repository: 'acme/widgets', number: 7 }),
      step(call('post_review', { version: 1 })),
      step(call('write', { path: 'artifacts/review.md', content: review })),
      step(call('submit_review', {})),
      step(fauxAssistantMessage('Reviewed.')),
      step(call('post_review', { version: 1 })),
      step(call('post_review', { version: 1 })),
      step(fauxAssistantMessage('Posted.')),
    ];
  });
  assert.equal(done.status === 'done' && done.answer, 'Posted.');
  assert.match(seen[1], /is not sealed; call submit_review first/, 'nothing posts before it is checked');
  assert.match(seen[3], /github\.review v1: 100% \(github\.reviewer@3\), passed/);
  assert.match(seen[4], /Review v1 is cleared to post but has not been posted/, 'finishing before posting is refused');
  assert.match(seen[6], /review v1 was already posted; nothing was sent again/);

  assert.equal(service.posted.length, 1);
  assert.deepEqual(service.posted[0], { owner: 'acme', repo: 'widgets', number: 7, commitId: head, event: 'APPROVE', body: review });

  const doc = await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT);
  const verdict = doc!.kinds['github.review'].versions[0].verdict as any;
  assert.equal(verdict.passed, true);
  assert.deepEqual(doc!.kinds['github.review'].versions[0].links, [{ label: 'Posted', url: 'https://github.com/acme/widgets/pull/7#review-9' }]);
  const resolvers = new Set(verdict.checks.flatMap((c: any) => Object.keys(c.resolvers)));
  assert.deepEqual([...resolvers].sort(), ['clef', 'code']);
  assert.ok(clefUnits.includes('review+description+ci+linkedIssues+diff'), 'the change unit carries the diff and PR context');
  assert.ok(clefUnits.includes('finding+citedCode'), 'each finding is judged with its cited code');
  await session.close();
});

test('a review that quotes code the repository does not have is sent back before it can post', async () => {
  const misquoted = review.replace('a named constant would say why the value is 2.', 'it still says `export const a = 3;`, which is the old value.');
  const { done, seen, service, session } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [
      call('checkout_pr', { repository: 'acme/widgets', number: 7 }),
      step(call('write', { path: 'artifacts/review.md', content: misquoted })),
      step(call('submit_review', {})),
      step(call('write', { path: 'artifacts/review.md', content: review })),
      step(call('submit_review', {})),
      step(call('post_review', { version: 2 })),
      step(fauxAssistantMessage('Posted.')),
    ];
  });
  assert.equal(done.status === 'done' && done.answer, 'Posted.');
  assert.match(seen[2], /gate claims_grounded\.quotes_exist[^"]*finding-1 quotes code that is not in the repository: `export const a = 3;`/);
  assert.equal(service.posted.length, 1);
  assert.equal(service.posted[0].body, review, 'only the corrected version went out');
  await session.close();
});

test('a run that revises after posting sends one review, and may finish without posting the revision', async () => {
  const revised = review.replace('approve.', 'approve, with one nit.');
  const { done, seen, service, session } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [
      call('checkout_pr', { repository: 'acme/widgets', number: 7 }),
      step(call('write', { path: 'artifacts/review.md', content: review })),
      step(call('submit_review', {})),
      step(call('post_review', { version: 1 })),
      step(call('write', { path: 'artifacts/review.md', content: revised })),
      step(call('submit_review', {})),
      step(call('post_review', { version: 2 })),
      step(fauxAssistantMessage('Posted.')),
      call('checkout_pr', { repository: 'acme/widgets', number: 7 }),
      call('post_review', { version: 2 }),
      fauxAssistantMessage('Re-reviewed.'),
    ];
  });
  assert.equal(done.status === 'done' && done.answer, 'Posted.', 'the unposted v2 does not hold the run open');
  assert.match(seen[6], /review v1 was already posted for this checkout; a run posts one review, so v2 was not sent/);
  assert.equal(service.posted.length, 1);
  assert.equal(service.posted[0].body, review);

  const again = await session.submit('/review', 'gh-2', true);
  assert.equal(again.status === 'done' && again.answer, 'Re-reviewed.');
  assert.equal(service.posted.length, 2, 'a later run checks out afresh and may post');
  assert.equal(service.posted[1].body, revised);
  await session.close();
});

test('a PR pushed to while it is reviewed is not posted against the old head; the run checks out the new one', async () => {
  let moved = '';
  const { done, seen, service, head, session } = await reviewRun((seen, advance) => {
    const step = recorder(seen);
    return [
      call('checkout_pr', { repository: 'acme/widgets', number: 7 }),
      step(call('write', { path: 'artifacts/review.md', content: review })),
      step(call('submit_review', {})),
      (_c: unknown) => { moved = advance(); return call('post_review', { version: 1 }); },
      step(call('checkout_pr', { repository: 'acme/widgets', number: 7 })),
      step(call('post_review', { version: 1 })),
      step(fauxAssistantMessage('Posted.')),
    ];
  });
  assert.equal(done.status === 'done' && done.answer, 'Posted.');
  assert.match(seen[2], /moved from \w+ to \w+; call checkout_pr again/);
  assert.notEqual(moved, head);
  assert.deepEqual(service.posted.map((p) => p.commitId), [moved]);
  await session.close();
});

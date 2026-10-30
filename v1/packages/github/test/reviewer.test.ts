import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, createProvider, envApiKeyAuth, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { typesafeSystemOneApi } from '@earendil-works/pi-ai/api/typesafe-system-one.lazy';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { githubSessionExtensions } from '../src/index.ts';
import { cleanup, fakeService, listen, remotes, tmp } from './fixtures.ts';

after(cleanup);

const PITFALLS = /toolchain_as_defect|contradicted|no_reason_given|preference_only|wrong_output_below_blocking/;
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

test('a reviewer run checks out the PR, has its review judged, and posts exactly the checked version', async () => {
  process.env.QUACK_CLEF_KEY = 'none';
  const { base, head } = await remotes();
  const service = await fakeService(head);
  const clefUrl = await fakeClef();

  const seen: string[] = [];
  const step = (msg: ReturnType<typeof fauxAssistantMessage>) => (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), msg);
  const call = (name: string, args: Record<string, unknown>) => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });
  const faux = fauxProvider({ models: [{ id: 'worker' }] });
  faux.setResponses([
    call('checkout_pr', { repository: 'acme/widgets', number: 7 }),
    step(call('post_review', { version: 1 })),
    step(call('write', { path: 'artifacts/review.md', content: review })),
    step(call('submit_review', {})),
    step(call('post_review', { version: 1 })),
    step(fauxAssistantMessage('Posted.')),
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const clef = { id: 'clef', name: 'clef', type: 'classifier', api: 'typesafe-system-one', provider: 'clef', baseUrl: clefUrl, input: ['text'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 8192 };
  models.setProvider(createProvider({ id: 'clef', baseUrl: clefUrl, auth: { apiKey: envApiKeyAuth('clef', ['QUACK_CLEF_KEY']) }, models: [clef] as never, classifiers: { 'typesafe-system-one': typesafeSystemOneApi() } }));
  const m = faux.getModel();
  const extensions = [CodingTools, ...githubSessionExtensions({ serviceUrl: service.url, serviceToken: 'svc', gitBaseUrl: base, classifier: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 } })];
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });

  const done = await session.submit('Review pull request acme/widgets#7', 'gh-1', true);
  assert.equal(done.status === 'done' && done.answer, 'Posted.');
  assert.match(seen[1], /is not sealed; call submit_review first/, 'nothing posts before it is checked');
  assert.match(seen[3], /github\.review v1: 100% \(github\.reviewer@1\), passed/);

  assert.equal(service.posted.length, 1);
  assert.deepEqual(service.posted[0], { owner: 'acme', repo: 'widgets', number: 7, commitId: head, event: 'APPROVE', body: review });

  const doc = await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT);
  const verdict = doc!.kinds['github.review'].versions[0].verdict as any;
  assert.equal(verdict.passed, true);
  const resolvers = new Set(verdict.checks.flatMap((c: any) => Object.keys(c.resolvers)));
  assert.deepEqual([...resolvers].sort(), ['clef', 'code']);
  assert.ok(clefUnits.includes('review+description+ci+linkedIssues+diff'), 'the change unit carries the diff and PR context');
  assert.ok(clefUnits.includes('finding+citedCode'), 'each finding is judged with its cited code');
  await session.close();
});

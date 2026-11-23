import assert from 'node:assert/strict';
import { manifest as judgeManifest } from '@quack/judge';
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
import { steerInput } from '../src/service.ts';
import { cleanup, fakeService, listen, remotes, sh, tmp } from './fixtures.ts';

after(cleanup);

const inlined = `The change does what issue #3 asks; approve.

1 nit inline.

<details><summary>What I checked</summary>

**Verified**

- read app.ts:1 on the PR head: \`export const a = 2;\`

</details>`;
const unanchored = inlined.replace('1 nit inline.', 'Not anchored to a diff line:\n- nit: app.ts:1 — a named constant would say why the value is 2.');
// The questions asked as the defect, which a good review answers "no", by their text: a check id never reaches Clef.
const PITFALLS = /toolchain or network failure|contradict this finding|only assert that something is wrong|rest only on preference|fail to name something that goes wrong|though not labelled blocking|claim a result/;
const clefUnits: string[] = [];
async function fakeClef() {
  return `${await listen(
    createServer(async (req, res) => {
      let raw = '';
      for await (const c of req) raw += c;
      const { state, questions } = JSON.parse(raw);
      clefUnits.push(Object.keys(state).join('+'));
      const answers = Object.fromEntries(Object.entries(questions as Record<string, { instructions: string }>).map(([q, { instructions }]) => [q, { type: 'noul', noul: PITFALLS.test(instructions) ? 0.04 : 0.96 }]));
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

async function reviewRun(responses: (seen: string[], advance: () => string, steer: (text: string) => Promise<unknown>) => FauxResponseStep[], pr: Record<string, unknown> = {}, faults: { loseFirstReview?: boolean } = {}) {
  process.env.QUACK_CLEF_KEY = 'none';
  const { base, head } = await remotes();
  const live: Record<string, unknown> = { headSha: head, ...pr };
  const service = await fakeService(head, live, undefined, faults);
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
  // A steer as the service sends one mid-run: the session places it at the run's next tool boundary.
  faux.setResponses(responses(seen, advance, (text) => session.submit(text, 'gh-steer', false, 'steer')));
  const models = buildModels(parseDeployment({ defaultModel: 'clef/clef', providers: { clef: { baseUrl: clefUrl, api: 'systemone', apiKeyEnv: 'QUACK_CLEF_KEY', models: { clef: { contextWindow: 8192 } } } } }));
  models.setProvider(faux.provider);
  models.setProvider(fauxJudge().provider);
  const m = faux.getModel();
  // The reviewer agent enables the judge, which installs before github as its resolved order puts it.
  const judge = await judgeManifest.session!({ maxRevisions: 2, decisionModel: { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 }, judgeModel: { provider: 'judge', modelId: 'j' } }, {}, []);
  const extensions = [CodingTools, ...judge, ...githubSessionExtensions({ serviceUrl: service.url, serviceToken: 'svc', gitBaseUrl: base })];
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  const done = await session.submit('Review pull request acme/widgets#7', 'gh-1', true);
  return { done, seen, service, head, session, live };
}

// An open question calls for `comment`, so approving it fails the verdict gate; `n` makes each draft a new version.
const asksButApproves = (n: number) => review.replace('- nit: app.ts:1 — a named constant would say why the value is 2.', `- question: app.ts:1 — why is the value 2 (draft ${n})?`);

// The judge model answers the questions that skip Clef as a sound review earns: "no" to a defect, "yes" otherwise.
function fauxJudge() {
  const judge = fauxProvider({ provider: 'judge', models: [{ id: 'j' }] });
  const decide = (question: string) => ({ choice: PITFALLS.test(question) ? 'no' : 'yes', confidence: 0.9, reason: 'as the review says' });
  const answer: FauxResponseStep = (c) => {
    const text = (c.messages.at(-1) as { content: string }).content;
    const batch = [...text.matchAll(/^- q(\d+): (.*)$/gm)].map((m) => [`q${m[1]}`, decide(m[2])]);
    return fauxAssistantMessage(JSON.stringify(batch.length ? Object.fromEntries(batch) : decide(text)));
  };
  judge.setResponses(Array.from({ length: 200 }, () => answer));
  return judge;
}

const call = (name: string, args: Record<string, unknown>) => fauxAssistantMessage(fauxToolCall(name, args as never), { stopReason: 'toolUse' });
const recorder = (seen: string[]) => (msg: ReturnType<typeof fauxAssistantMessage>) => (c: { messages: unknown[] }) => (seen.push(JSON.stringify(c.messages.at(-1))), msg);

const URL7 = 'https://github.com/acme/widgets/pull/7#pullrequestreview-9';
const write = (content: string) => call('write', { path: 'artifacts/review.md', content });
const checkout = () => call('checkout_pr', { repository: 'acme/widgets', number: 7 });

test('a passing submit posts the checked version once, returns the link, and a repeat submit sends nothing more', async () => {
  const { done, seen, service, head, session } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [checkout(), step(write(review)), step(call('submit_review', {})), step(call('submit_review', {})), step(fauxAssistantMessage('Reviewed.'))];
  });
  assert.equal(done.status === 'done' && done.answer, 'Reviewed.');
  assert.match(seen[2], /github\.review v1: 100% \(github\.reviewer@10\), passed\./);
  assert.match(seen[2], new RegExp(`Posted as APPROVE: ${URL7.replace(/[/.#]/g, '\\$&')}`));
  assert.match(seen[3], /Review v1 was already posted; nothing was sent again/);
  assert.equal(service.posted.length, 1);
  // The nit cites app.ts:1, which the PR changes: it goes inline and the body lists it by title.
  assert.match(service.posted[0].requestId, /^.+\/v1$/, 'the post names its conversation and version');
  assert.deepEqual(service.posted[0], { commitId: head, event: 'APPROVE', body: inlined, requestId: service.posted[0].requestId, fallbackBody: unanchored, comments: [{ path: 'app.ts', line: 1, body: '**nit**: a named constant would say why the value is 2.' }] });

  const doc = await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT);
  const verdict = doc!.kinds['github.review'].versions[0].verdict as any;
  assert.equal(verdict.passed, true);
  assert.deepEqual(doc!.kinds['github.review'].versions[0].links, [{ label: 'Posted', url: URL7 }]);
  const resolvers = new Set(verdict.checks.flatMap((c: any) => Object.keys(c.resolvers)));
  assert.deepEqual([...resolvers].sort(), ['clef', 'code', 'j'], 'contradicted and approval_grounded skip Clef for the judge model');
  assert.ok(clefUnits.includes('context+items'), 'findings are packed with the CI result they share');
  assert.ok(clefUnits.includes('review'), 'the takeaway is judged on the review whole');
  await session.close();
});

test('a post whose answer was lost after GitHub took it is resubmitted under the same delivery id, so the service returns the first post', async () => {
  const { done, seen, service, session } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [checkout(), step(write(review)), step(call('submit_review', {})), step(call('submit_review', {})), step(fauxAssistantMessage('Posted.'))];
  }, {}, { loseFirstReview: true });
  assert.equal(done.status === 'done' && done.answer, 'Posted.');
  assert.match(seen[2], /Posting failed: 502 .*Call the submit tool again to retry\./);
  assert.match(seen[3], /Posted as APPROVE/);
  assert.deepEqual(service.posted.map((p) => p.requestId), [service.posted[0].requestId, service.posted[0].requestId]);
  await session.close();
});

test('a failing submit posts nothing; the corrected version is the one that goes out', async () => {
  const { done, seen, service, session } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [checkout(), step(write(asksButApproves(1))), step(call('submit_review', {})), step(write(review)), step(call('submit_review', {})), step(fauxAssistantMessage('Posted.'))];
  });
  assert.equal(done.status === 'done' && done.answer, 'Posted.');
  assert.match(seen[2], /gate structured_verdict\.verdict_matches_labels[^"]*the findings call for comment, the verdict says approve/);
  assert.doesNotMatch(seen[2], /Posted as/);
  assert.equal(service.posted.length, 1);
  assert.equal(service.posted[0].body, inlined);
  await session.close();
});

test('a review released at the revision limit posts with its failures marked', async () => {
  const { done, seen, service, session } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [checkout(), step(write(asksButApproves(1))), step(call('submit_review', {})), step(write(asksButApproves(2))), step(call('submit_review', {})), step(write(asksButApproves(3))), step(call('submit_review', {})), step(fauxAssistantMessage('Released.'))];
  });
  assert.equal(done.status === 'done' && done.answer, 'Released.');
  assert.doesNotMatch(seen[2], /Posted as/);
  assert.doesNotMatch(seen[4], /Posted as/);
  assert.match(seen[6], /The revision limit is reached\./);
  assert.match(seen[6], /Posted as APPROVE: /);
  assert.equal(service.posted.length, 1);
  assert.match(service.posted[0].body, /\*\*Not fully checked:\*\* the revision limit was reached\. Checks that did not pass:\n- /);
  assert.match(service.posted[0].comments[0].body, /draft 3/);
  assert.ok(service.posted[0].body.startsWith('The change does what issue #3 asks'));
  await session.close();
});

test('a PR pushed to while it is reviewed is not posted against the old head; resubmitting the old version stays refused, a new version posts', async () => {
  let moved = '';
  const { done, seen, service, head, session } = await reviewRun((seen, advance) => {
    const step = recorder(seen);
    return [
      checkout(),
      step(write(review)),
      (_c: unknown) => { moved = advance(); return call('submit_review', {}); },
      step(checkout()),
      step(call('submit_review', {})),
      step(write(review.replace('approve.', 'approve on the new head.'))),
      step(call('submit_review', {})),
      step(fauxAssistantMessage('Posted.')),
    ];
  });
  assert.equal(done.status === 'done' && done.answer, 'Posted.');
  assert.match(seen[1], /Nothing was posted: the pull request moved from \w+ to \w+; check out again with checkout_pr/);
  assert.notEqual(moved, head);
  assert.match(seen[3], new RegExp(`Nothing was posted: v1 reviewed ${head}; review the new head and submit a new version`));
  assert.match(seen[5], /Posted as APPROVE: /);
  assert.deepEqual(service.posted.map((p) => p.commitId), [moved]);
  assert.match(service.posted[0].body, /approve on the new head/);
  await session.close();
});

test('a push steered into a running review: the old head is not posted, and the same run checks out and posts the new head', async () => {
  let moved = '';
  const { done, seen, service, session } = await reviewRun((seen, advance, steer) => {
    const step = recorder(seen);
    return [
      checkout(),
      step(write(review)),
      async () => { moved = advance(); await steer(steerInput({ owner: 'acme', repo: 'widgets', number: 7, pushedHead: moved })); return call('submit_review', {}); },
      (c) => (seen.push(JSON.stringify(c.messages.slice(-2))), checkout()),
      step(write(review.replace('approve.', 'approve on the new head.'))),
      step(call('submit_review', {})),
      step(fauxAssistantMessage('Posted.')),
    ];
  });
  assert.equal(done.status === 'done' && done.answer, 'Posted.', 'one run');
  assert.match(seen[1], new RegExp(`Nothing was posted: the pull request moved .*"role":"user".*its head is now ${moved}\\. Call checkout_pr again`), 'the steer follows the refused submit');
  assert.deepEqual(service.posted.map((p) => p.commitId), [moved]);
  await session.close();
});

test('a re-review after a push may not end on the review posted for the old head: the run is sent back to submit one for the new head', async () => {
  let push = () => '';
  const { seen, service, head, session } = await reviewRun((seen, advance) => {
    push = advance;
    const step = recorder(seen);
    return [
      checkout(), step(write(review)), step(call('submit_review', {})), step(fauxAssistantMessage('Posted.')),
      checkout(), step(write(review.replace('approve.', 'approve on the new head.'))),
      // The model claims the post without calling submit_review, as the reviewer did on fagerbergj/quack#2386.
      step(fauxAssistantMessage('Re-review posted as approve.')),
      step(call('submit_review', {})), step(fauxAssistantMessage('Posted.')),
    ];
  });
  const moved = push();
  const again = await session.submit('Review pull request acme/widgets#7 (new commits pushed)', 'gh-2', true);
  assert.equal(again.status === 'done' && again.answer, 'Posted.');
  assert.match(seen[5], new RegExp(`github\\.review v1 was written against ${head}, not ${moved}\\. Write artifacts/review\\.md for ${moved} and call submit_review before you finish\\.`));
  assert.deepEqual(service.posted.map((p) => p.commitId), [head, moved]);
  assert.match(service.posted[1].body, /approve on the new head/);
  await session.close();
});

// A relabel after a reply on the reviewed head: the same head, so only the new reply makes the checkout look again.
async function rereview(script: (step: ReturnType<typeof recorder>) => FauxResponseStep[]) {
  const thread = { id: 1, path: 'app.ts', line: 1, commit: 'a'.repeat(40), at: '2026-11-09T22:58:17Z', outdated: false, body: '**question**: is the route exposed?', replies: [] as object[], repliesCut: 0 };
  const feedback = { reviews: [], comments: [], ownThreads: [thread], ownThreadsCut: 0 };
  const run = await reviewRun((seen) => {
    const step = recorder(seen);
    return [checkout(), step(write(review)), step(call('submit_review', {})), step(fauxAssistantMessage('Posted.')), checkout(), ...script(step)];
  }, { feedback });
  run.live.feedback = { ...feedback, ownThreads: [{ ...thread, replies: [{ author: 'dev', at: '2026-11-09T23:01:23Z', body: 'It 302s to the login.' }] }] };
  const again = await run.session.submit('Review pull request acme/widgets#7 (label quack:review)', 'gh-2', true);
  return { ...run, again };
}
const resolved = review.replace('approve.', 'approve; the earlier question is resolved.');

test('a re-review of an unchanged head that writes a new review and stops without submitting is sent back to submit it', async () => {
  const { again, seen, service, head, session } = await rereview((step) => [
    step(write(resolved)),
    // The model claims the post without calling submit_review, as the reviewer did on fagerbergj/quack#2417.
    step(fauxAssistantMessage('Approve, 0 findings.')),
    step(call('submit_review', {})), step(fauxAssistantMessage('Posted.')),
  ]);
  assert.equal(again.status === 'done' && again.answer, 'Posted.');
  assert.match(seen[5], /artifacts\/review\.md changed after github\.review v1 and this run has not submitted it, so the change is not delivered: v1 is still the newest version\. Call submit_review to submit it before you finish\./);
  assert.deepEqual(service.posted.map((p) => p.commitId), [head, head]);
  assert.match(service.posted[1].body, /the earlier question is resolved/);
  await session.close();
});

test('a re-review of an unchanged head that writes a new review and submits it posts it and ends', async () => {
  const { again, seen, service, head, session } = await rereview((step) => [step(write(resolved)), step(call('submit_review', {})), step(fauxAssistantMessage('Posted.'))]);
  assert.equal(again.status === 'done' && again.answer, 'Posted.');
  assert.match(seen[5], /github\.review v2: .*passed\..*Posted as APPROVE: /);
  assert.equal(seen.length, 6, 'the run ends on its answer, with nothing sent back');
  assert.deepEqual(service.posted.map((p) => p.commitId), [head, head]);
  await session.close();
});

test('a reply linking a review the app did not post is sent back with the link the submit result gave', async () => {
  const { again, seen, session } = await rereview((step) => [
    step(write(resolved)), step(call('submit_review', {})),
    // The model's own review id, not the one GitHub returned, as on fagerbergj/quack#2417.
    step(fauxAssistantMessage('Approve: https://github.com/acme/widgets/pull/7#pullrequestreview-5949693520')),
    step(fauxAssistantMessage(`Approve: ${URL7}`)),
  ]);
  assert.equal(again.status === 'done' && again.answer, `Approve: ${URL7}`);
  assert.match(seen[6], new RegExp(`Your reply links https://github\\.com/acme/widgets/pull/7#pullrequestreview-5949693520, but this session posted no such review\\. The reviews it posted: ${URL7.replace(/[/.#]/g, '\\$&')}\\. Reply again without that link\\.`));
  await session.close();
});

test('a request naming an old head reviews the live head and says so', async () => {
  const { seen, service, head, session } = await reviewRun((seen) => [
    call('checkout_pr', { repository: 'acme/widgets', number: 7, head: 'a30f00a' }),
    recorder(seen)(fauxAssistantMessage('Stopped.')),
  ]);
  assert.match(seen[0], new RegExp(`The request was for a30f00a, an older head; this reviews the live head ${head}.*Checked out ${head}`));
  assert.equal(service.posted.length, 0);
  await session.close();
});

test('a re-review checkout shows the reviewer its own earlier inline comments with their replies', async () => {
  const ownThreads = [{ id: 1, path: 'app.ts', line: 1, commit: 'a'.repeat(40), at: '2026-11-09T22:58:17Z', outdated: false, body: '**question**: is the route exposed?', replies: [{ author: 'dev', at: '2026-11-09T23:01:23Z', body: 'Checked on prod: it 302s to the login.' }], repliesCut: 0 }];
  const { seen, session } = await reviewRun((seen) => [checkout(), recorder(seen)(fauxAssistantMessage('Stopped.'))], { feedback: { reviews: [], comments: [], ownThreads, ownThreadsCut: 0 } });
  assert.match(seen[0], /Your earlier inline comments on this pull request, newest first.*### app\.ts:1, commented on aaaaaaaa at 2026-11-09T22:58:17Z; the commented line is unchanged in the current diff\\n\*\*question\*\*: is the route exposed\?\\n- reply by dev at 2026-11-09T23:01:23Z:\\n  Checked on prod/);
  await session.close();
});

test('a second version after the posted one is refused and not sealed; a later request for the reviewed head ends without checkout unless the description or CI changed', async () => {
  const revised = review.replace('approve.', 'approve, with one nit.');
  const { done, seen, service, session, live } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [
      checkout(),
      step(write(review)),
      step(call('submit_review', {})),
      step(write(revised)),
      step(call('submit_review', {})),
      step(fauxAssistantMessage('Posted.')),
      checkout(),
      step(fauxAssistantMessage('Nothing to do.')),
      checkout(),
      step(fauxAssistantMessage('Looking again.')),
    ];
  });
  assert.equal(done.status === 'done' && done.answer, 'Posted.');
  assert.match(seen[4], /github\.review v1 passed and is delivered, so it is final for this run/);
  const doc = await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT);
  assert.equal(doc!.kinds['github.review'].versions.length, 1, 'the revision is not sealed');
  assert.equal(service.posted.length, 1);
  assert.equal(service.posted[0].body, inlined);

  const again = await session.submit('/review', 'gh-2', true);
  assert.equal(again.status === 'done' && again.answer, 'Nothing to do.');
  assert.match(seen[5], /already reviewed \w{40}: nothing to do this turn/);
  assert.doesNotMatch(seen[5], /Checked out/);
  assert.equal(service.posted.length, 1);

  live.body = 'Fixes #3, described right this time';
  await session.submit('/review', 'gh-3', true);
  assert.match(seen[6], /Checked out \w{40}.*described right this time/);
  await session.close();
});

test('a reply to an own comment on the reviewed head is worth another look: the next request checks out and shows it', async () => {
  const thread = { id: 1, path: 'app.ts', line: 1, commit: 'a'.repeat(40), at: '2026-11-09T22:58:17Z', outdated: false, body: '**question**: is the route exposed?', replies: [] as object[], repliesCut: 0 };
  const feedback = { reviews: [], comments: [], ownThreads: [thread], ownThreadsCut: 0 };
  const { seen, session, live } = await reviewRun((seen) => {
    const step = recorder(seen);
    return [checkout(), step(write(review)), step(call('submit_review', {})), step(fauxAssistantMessage('Posted.')), checkout(), step(fauxAssistantMessage('Nothing to do.')), checkout(), step(fauxAssistantMessage('Looking again.'))];
  }, { feedback });
  await session.submit('/review', 'gh-2', true);
  assert.match(seen[3], /already reviewed \w{40}: nothing to do this turn/);
  live.feedback = { ...feedback, ownThreads: [{ ...thread, replies: [{ author: 'dev', at: '2026-11-09T23:01:23Z', body: 'Checked on prod: it 302s to the login.' }] }] };
  await session.submit('/review', 'gh-3', true);
  assert.match(seen[4], /Checked out \w{40}.*- reply by dev at 2026-11-09T23:01:23Z:\\n  Checked on prod/);
  await session.close();
});

// A looping model: its first turn is `first`, then it alternates a fresh draft with a submit, forever.
const loop = (first: ReturnType<typeof call>, seen: string[], counter: { turns: number }): FauxResponseStep[] => [
  ...Array.from({ length: 60 }, (_, i) => (_c: unknown) => {
    counter.turns++;
    return i === 0 ? first : i % 2 === 1 ? write(review.replace('constant', `constant${i}`)) : call('submit_review', {});
  }),
  recorder(seen)(fauxAssistantMessage('never')),
];

test('a closed PR ends the run at the first submit: it is refused for good, not sent back for revision', async () => {
  const counter = { turns: 0 };
  const { done, service, session } = await reviewRun((seen) => loop(checkout(), seen, counter), { state: 'closed' });
  assert.equal(done.status, 'done');
  assert.equal(counter.turns, 3, 'checkout, one draft, one refused submit');
  assert.equal(service.posted.length, 0);
  await session.close();
});

test('submits that keep changing stop at the revision limit: the version released there is final, and the second change after it ends the run', async () => {
  const counter = { turns: 0 };
  const steps = Array.from({ length: 40 }, (_, i) => (_c: unknown) => {
    counter.turns++;
    return i === 0 ? checkout() : i % 2 === 1 ? write(asksButApproves(i)) : call('submit_review', {});
  });
  const { done, service, session } = await reviewRun(() => steps);
  assert.equal(done.status, 'done');
  assert.equal(counter.turns, 1 + 2 * 5, 'checkout, three judged draft and submit pairs, then two refused');
  assert.equal(service.posted.length, 1, 'v3 went out at the limit with its failures marked');
  await session.close();
});

test('non-https and scheme-less made-up review links are caught, and repeated bad links stop at the retry bound', async () => {
  const { again, seen, session } = await rereview((step) => [
    step(write(resolved)), step(call('submit_review', {})),
    step(fauxAssistantMessage('Approve: http://github.com/acme/widgets/pull/7#pullrequestreview-111')),
    step(fauxAssistantMessage('Approve: github.com/acme/widgets/pull/7#pullrequestreview-222')),
    step(fauxAssistantMessage('Approve: github.com/acme/widgets/pull/7#pullrequestreview-333')),
  ]);
  assert.equal(again.status === 'done' && again.answer, 'Approve: github.com/acme/widgets/pull/7#pullrequestreview-333');
  assert.match(seen[6], /Your reply links http:\/\/github\.com\/acme\/widgets\/pull\/7#pullrequestreview-111/);
  assert.match(seen[7], /Your reply links github\.com\/acme\/widgets\/pull\/7#pullrequestreview-222/);
  assert.equal(seen.length, 8, 'two send-backs then the third reply ends the run');
  await session.close();
});

test('a review file edited via bash is caught as unsubmitted, and repeated unsubmitted yields stop at the retry bound', async () => {
  const { again, seen, service, session } = await rereview((step) => [
    step(call('bash', { command: `printf '%s\n' ${JSON.stringify(resolved)} > artifacts/review.md` })),
    step(fauxAssistantMessage('Approve 1.')),
    step(fauxAssistantMessage('Approve 2.')),
    step(fauxAssistantMessage('Approve 3.')),
  ]);
  assert.equal(again.status === 'done' && again.answer, 'Approve 3.');
  assert.match(seen[5], /artifacts\/review\.md changed after github\.review v1/);
  assert.match(seen[6], /artifacts\/review\.md changed after github\.review v1/);
  assert.equal(seen.length, 7, 'two send-backs then the third reply ends the run');
  assert.equal(service.posted.length, 1, 'unsubmitted bash edit was never posted');
  await session.close();
});

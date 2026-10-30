import assert from 'node:assert/strict';
import { readFile, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, Type } from '@earendil-works/pi-ai';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { Artifacts, artifactsExtension, seal, type ArtifactKind } from '@quack/sdk';
import { openSession } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-artifacts-'));
after(() => rm(dir, { recursive: true, force: true }));

const review: ArtifactKind = {
  kind: 'github.review',
  path: 'artifacts/review.md',
  validate: (text) => [...(text.includes('## Verdict') ? [] : ['missing a "## Verdict" section']), ...(text.length > 20 ? [] : ['shorter than 20 characters'])],
};

const submit = defineTool({
  name: 'submit_review',
  description: 'Seal the review for checking',
  parameters: Type.Object({}),
  replay: 'safe',
  execute: async (_args, api, ctx) => {
    const sealed = await seal(api, review, ctx);
    return { output: [{ type: 'text', text: `sealed ${sealed.kind} v${sealed.version}` }] };
  },
});

test('writes are validated early, and sealing snapshots the file into the log', async () => {
  const seen: string[] = [];
  let sealedAfterFirst = -1;
  const lastResult = (context: { messages: unknown[] }) => seen.push(JSON.stringify(context.messages.at(-1)));
  const good = '# Review\n\nLooks right.\n\n## Verdict\n\nApprove.\n';
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([
    fauxAssistantMessage(fauxToolCall('write', { path: 'artifacts/review.md', content: 'LGTM' }), { stopReason: 'toolUse' }),
    (c) => (lastResult(c), fauxAssistantMessage(fauxToolCall('submit_review', {}), { stopReason: 'toolUse' })),
    (c) => (lastResult(c), fauxAssistantMessage(fauxToolCall('write', { path: 'artifacts/review.md', content: good }), { stopReason: 'toolUse' })),
    (c) => (lastResult(c), fauxAssistantMessage(fauxToolCall('submit_review', {}), { stopReason: 'toolUse' })),
    async (c) => {
      lastResult(c);
      const doc = await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT);
      sealedAfterFirst = doc!.kinds['github.review'].versions.length;
      return fauxAssistantMessage(fauxToolCall('submit_review', {}), { stopReason: 'toolUse' });
    },
    (c) => (lastResult(c), fauxAssistantMessage('Submitted.')),
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  let session: Awaited<ReturnType<typeof openSession>>;
  session = await openSession({
    dir,
    models,
    agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, artifactsExtension([review]), defineExtension({ name: 'gh', tools: [submit] })] },
  });

  const done = await session.submit('Write and submit the review.', 'art-1', true);
  assert.equal(done.status === 'done' && done.answer, 'Submitted.');

  assert.match(seen[0], /does not yet match github\.review/, 'the bad write reports its problems');
  assert.match(seen[0], /missing a \\"## Verdict\\" section.*shorter than 20 characters/s, 'every problem at once');
  assert.match(seen[1], /does not match github\.review/, 'sealing refuses an invalid file');
  assert.doesNotMatch(seen[2], /does not yet match/, 'a valid write is quiet');
  assert.match(seen[3], /sealed github\.review v1/);
  assert.equal(sealedAfterFirst, 1, 'the first successful seal is in the log');
  assert.match(seen[4], /sealed github\.review v1/, 'an unchanged file keeps its version');

  const doc = await session.harness.snapshot(Artifacts, session.root.id, BACKGROUND_CONTEXT);
  const versions = doc!.kinds['github.review'].versions;
  assert.equal(versions.length, 1);
  assert.equal(versions[0].content, good);
  assert.equal(await readFile(join(session.workspace, 'artifacts/review.md'), 'utf8'), good);
  await session.close();
});

import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { artifactsExtension, gate, type ArtifactKind, type GateVerdict } from '@quack/sdk';
import { openSession } from '../src/index.ts';
import { transcriptOf } from '../src/export.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-seal-gates-'));
after(() => rm(dir, { recursive: true, force: true }));

const note: ArtifactKind = { kind: 'note', path: 'artifacts/note.md', tool: 'submit_note' };
const write = (content: string) => fauxAssistantMessage(fauxToolCall('write', { path: note.path, content }), { stopReason: 'toolUse' });
const submit = fauxAssistantMessage(fauxToolCall('submit_note', {}), { stopReason: 'toolUse' });
const lastText = (c: { messages: unknown[] }) => JSON.stringify(c.messages.at(-1));

// One session over a faux model; `script` gets each request's last message, the tool result the model reads.
async function run(name: string, script: Array<ReturnType<typeof fauxAssistantMessage>>) {
  const seen: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(script.map((reply) => (c: { messages: unknown[] }) => (seen.push(lastText(c)), reply)));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: join(dir, name), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [CodingTools, artifactsExtension([note])] } });
  const done = await session.submit('Write and submit the note.', `${name}-1`, true);
  const transcript = JSON.stringify(await transcriptOf(session.root));
  await session.close();
  return { done, seen, transcript };
}

test('the seal tool shows a continue as feedback, passes with every note in gate order, and reruns the gates on a re-seal', async (t) => {
  const calls: string[] = [];
  const check = async (e: { data: { version: number } }): Promise<GateVerdict> => (calls.push(`check v${e.data.version}`), e.data.version === 1 ? { kind: 'continue', feedback: 'Add a duck.' } : { kind: 'pass', note: 'Checked.' });
  const post = async (e: { data: { version: number } }): Promise<GateVerdict> => (calls.push(`post v${e.data.version}`), { kind: 'pass', note: 'Posted.' });
  const offs = [gate('artifact.sealed', check), gate('artifact.sealed', post)];
  t.after(() => offs.forEach((off) => off()));
  const { done, seen } = await run('chain', [write('a note'), submit, write('a note with a duck'), submit, submit, fauxAssistantMessage('Done.')]);
  assert.equal(done.status === 'done' && done.answer, 'Done.');
  assert.match(seen[2], /Add a duck\./);
  assert.match(seen[2], /"isError":true/);
  assert.match(seen[4], /Checked\.\\nPosted\./);
  assert.match(seen[5], /Checked\.\\nPosted\./, 'an unchanged file keeps its version');
  assert.deepEqual(calls, ['check v1', 'check v2', 'post v2', 'check v2', 'post v2'], 'a failed check stops the chain; a re-seal runs it again');
});

test('a deny ends the run after the seal round, with the reason as the result', async (t) => {
  t.after(gate('artifact.sealed', async () => ({ kind: 'deny', reason: 'Nothing to submit. This run is over.' })));
  const { done, seen, transcript } = await run('deny', [write('a note'), submit, fauxAssistantMessage('should not be asked')]);
  assert.equal(done.status, 'done');
  assert.match(transcript, /Nothing to submit\. This run is over\./);
  assert.equal(seen.length, 2, 'no request after the denied seal');
});

test('a kind without a tool gets no seal tool', async () => {
  const ext = artifactsExtension([{ kind: 'card', path: 'card.json' }, note]);
  assert.deepEqual((ext.tools ?? []).map((tool) => tool.name), ['submit_note']);
});

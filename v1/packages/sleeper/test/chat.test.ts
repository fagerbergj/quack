import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { contentText, createModels, fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseStep } from '@earendil-works/pi-ai';
import { openSession } from '@quack/session';
import { outcomeOf, WorkflowDoc, workflowSessionExtensions } from '@quack/workflows';
import { CHAT_PROMPT, createSleeperClient, lineupWorkflow, withChat } from '../src/index.ts';

const fixtures = JSON.parse(readFileSync(new URL('./fixtures/lineup.json', import.meta.url), 'utf8')) as Record<string, unknown>;
const AT = Date.parse('2026-11-06T15:00:00Z');
const INPUT = JSON.stringify({ league: 'L1', week: 9, owner: 'mallard' });
const NAME = 'sleeper.lineup';

const doFetch = (async (input: string) => {
  const path = new URL(input).pathname;
  return path in fixtures ? new Response(JSON.stringify(fixtures[path])) : new Response('null');
}) as unknown as typeof fetch;
const client = createSleeperClient({ fetch: doFetch, now: () => AT });
const decision = { slots: ['QB:100', 'RB1:101', 'RB2:103', 'WR1:104', 'WR2:105', 'TE:107', 'FLEX:106', 'DEF:SEA'].map((s) => ({ slot: s.split(':')[0], playerId: s.split(':')[1], why: 'Projected best.' })), summary: 'Start Dan Dash over Rob Rowe at RB2; every other slot is unchanged.' };

const dirs: string[] = [];
after(() => Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true }))));

// The model plays the job (a decision, then yes to every judge question) or the chat: it calls `tool` once, then answers with what the tool returned.
function open(chatTool?: { name: string; args: Record<string, string> }) {
  const seen: Array<{ system: string; tools: string[] }> = [];
  const step: FauxResponseStep = (c, options) => {
    const last = c.messages.at(-1)!;
    const asked = typeof last.content === 'string' ? last.content : '';
    if (/Decide every open slot/.test(asked)) return fauxAssistantMessage(JSON.stringify(decision));
    if (/Answer with one JSON object/.test(asked)) return fauxAssistantMessage(JSON.stringify({ choice: /bench a clearly better|mention weather/.test(asked) ? 'no' : 'yes', confidence: 1, reason: 'ok' }));
    const sys = c.messages[0] as unknown as { sections?: Record<string, string>; toolsAdded?: Array<{ name: string }> };
    seen.push({ system: sys.sections?.instructions ?? '', tools: (sys.toolsAdded ?? []).map((t) => t.name) });
    if (last.role === 'toolResult') return fauxAssistantMessage(`Read: ${contentText(last.content)}`);
    return fauxAssistantMessage(fauxToolCall(chatTool!.name, chatTool!.args), { stopReason: 'toolUse' });
  };
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(Array.from({ length: 80 }, () => step));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const workflow = withChat(lineupWorkflow({ client, now: () => AT }), { client });
  return mkdtemp(join(tmpdir(), 'quack-chat-')).then(async (dir) => {
    dirs.push(dir);
    const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, instructions: 'You are the lineup analyst.', extensions: workflowSessionExtensions({ workflow: NAME }, { [NAME]: workflow }) } });
    return { s, seen };
  });
}

const task = async (s: Awaited<ReturnType<typeof open>>['s']) => (await s.harness.snapshot(WorkflowDoc, s.root.id, ctx))!.task;

test('a scheduled start runs the workflow; a question after it gets a chat answer that read the sealed lineup', async () => {
  const { s, seen } = await open({ name: 'job_result', args: {} });
  const run = await s.submit(INPUT, 'schedule:lineup:1', true);
  assert.equal(run.status, 'done');
  const first = await task(s);

  const answer = await s.submit('Who starts at RB2 and why?', undefined, true);
  assert.equal(answer.status, 'done');
  assert.match((answer as { answer: string }).answer, /sleeper\.cards/);
  assert.match((answer as { answer: string }).answer, /Dan Dash/);
  assert.equal(await task(s), first, 'a question does not start a run');
  assert.equal(seen.at(-1)!.system.includes('answer follow-up questions'), true);
  assert.equal(seen.at(-1)!.system.includes('lineup analyst'), false, 'the job prompt does not reach the chat');
  assert.ok(['job_result', 'job_facts', 'player', 'rerun_job'].every((t) => seen.at(-1)!.tools.includes(t)));
  assert.equal(CHAT_PROMPT.split(/\s+/).length < 140, true);
  assert.match(CHAT_PROMPT, /no preamble/);
  assert.match(CHAT_PROMPT, /markdown links/);
  assert.match(CHAT_PROMPT, /never versions, check names, ids or scores/);
  await s.close();
});

test('the facts the job was built from are readable in chat', async () => {
  const { s } = await open({ name: 'job_facts', args: { step: 'facts' } });
  await s.submit(INPUT, 'start:L1:1', true);
  const answer = await s.submit('Why was Rob Rowe benched?', undefined, true);
  assert.match((answer as { answer: string }).answer, /Rob Rowe/);
  assert.match((answer as { answer: string }).answer, /"baseline"/);
  await s.close();
});

test('a player lookup returns Sleeper status', async () => {
  const { s } = await open({ name: 'player', args: { name: 'rob rowe' } });
  await s.submit(INPUT, 'start:L1:1', true);
  const answer = await s.submit('Is Rob Rowe playing?', undefined, true);
  assert.match((answer as { answer: string }).answer, /"injury_status":"Questionable"/);
  await s.close();
});

test('asking to redo the job starts a fresh run on the same input', async () => {
  const { s } = await open({ name: 'rerun_job', args: {} });
  await s.submit(INPUT, 'start:L1:1', true);
  const first = (await task(s))!;
  const answer = await s.submit('Re-run the lineup with Irving in', undefined, true);
  assert.match((answer as { answer: string }).answer, /Started run/);
  const second = (await task(s))!;
  assert.notEqual(second, first);
  assert.equal((await outcomeOf(s.harness, second)).status, 'completed');
  await s.close();
});

test('a start with a requestId still runs the workflow when the job has a chat', async () => {
  const { s } = await open({ name: 'job_result', args: {} });
  const a = await s.submit(INPUT, 'start:L1:1', true);
  const b = await s.submit(INPUT, 'start:L1:2', true);
  assert.equal(a.status, 'done');
  assert.equal(b.status, 'done');
  assert.notEqual(a.submissionId, b.submissionId);
  await s.close();
});

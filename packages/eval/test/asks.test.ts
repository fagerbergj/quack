import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { mkdtemp } from 'node:fs/promises';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test, type TestContext } from 'node:test';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { decide, gated, person } from '@quack/sdk';
import { createSessionServer, openSession } from '@quack/session';
import { candidateAt, drive, eachArm, itemFrom, replay, type Answered, type Policy, type SessionRecord } from '../src/index.ts';

const ask = defineTool({
  name: 'ask', description: 'Ask the person', parameters: obj({}, { open: true }), replay: 'safe',
  execute: async (_a, api, c) => {
    const d = await decide(api, { name: 'color', question: 'Which color?', options: ['red', 'blue'], suggested: 1 }, [person()], c);
    return { output: [{ type: 'text', text: `color ${d?.choice}` }] };
  },
});
const propose = gated(defineTool({ name: 'propose', description: 'Propose the draft', parameters: obj({}, { open: true }), execute: async () => ({ output: [{ type: 'text', text: 'proposed' }] }) }));

/** A live session on a scripted model behind its HTTP server, as a replay's runtime serves it; `seen` is the last message of each model call. */
async function served(t: TestContext, script: string[]) {
  const seen: string[] = [];
  const step = (s: string) => (context: { messages: unknown[] }) => {
    seen.push(JSON.stringify(context.messages.at(-1)));
    return s.startsWith('call ') ? fauxAssistantMessage(fauxToolCall(s.slice(5), {}), { stopReason: 'toolUse' }) : fauxAssistantMessage(s);
  };
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(script.map(step) as never);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir: await mkdtemp(join(tmpdir(), 'quack-asks-')), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: 'demo', tools: [ask, propose] })] } });
  const server = createSessionServer(session).listen(0);
  await new Promise((r) => server.once('listening', r));
  t.after(async () => (await server.shutdown(), await session.close()));
  return { url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, seen };
}

// The largest delay a timer takes: these runs end when the session settles, never at a wall-clock deadline.
const NO_DEADLINE = 2 ** 31 - 1;
const run = async (url: string, inputs: string[], policy: Policy) => {
  const log: Answered[] = [];
  return { stopped: await drive({ url }, inputs, policy, log, Date.now() + NO_DEADLINE, undefined, 0), log };
};

test('questions take a recorded answer that fits, else the suggested option; approvals are approved, then rejected with the reason, and the next input waits for the run to settle', async (t) => {
  const s = await served(t, ['call ask', 'call ask', 'call propose', 'First done.', 'call propose', 'call propose', 'never']);
  const { stopped, log } = await run(s.url, ['Draft it.', 'Change it.'], { answers: ['red', 'green'], verdicts: ['approve', { reject: 'Name the caller.' }] });
  assert.deepEqual(log.map((a) => `${a.ask} ${a.about}: ${a.answer}`), [
    'question Which color?: red',
    'question Which color?: blue',
    'approval propose: approve',
    'approval propose: reject: Name the caller.',
    'approval propose: stop',
  ], 'green is no option, so the second question takes the suggested blue; the third proposal has no verdict left');
  assert.equal(stopped, true);
  assert.match(s.seen[1], /color red/);
  assert.match(s.seen[3], /proposed/);
  assert.match(s.seen[4], /Change it\./, 'the second input reached the model after the first run answered');
  assert.match(s.seen[5], /Rejected by \w+: Name the caller\./);
  assert.equal(s.seen.length, 6, 'the stopped run is not answered further');
});

test('with no verdicts the replay stops at the first proposal, and later inputs are not sent', async (t) => {
  const s = await served(t, ['call propose', 'never']);
  const { stopped, log } = await run(s.url, ['Draft it.', 'Change it.'], {});
  assert.deepEqual([stopped, log], [true, [{ ask: 'approval', about: 'propose', answer: 'stop' }]]);
  assert.equal(s.seen.length, 1);
});

test('an agent that asks nothing runs every input to the end', async (t) => {
  const s = await served(t, ['One.', 'Two.']);
  assert.deepEqual(await run(s.url, ['a', 'b'], { verdicts: ['stop'] }), { stopped: false, log: [] });
  assert.match(s.seen[1], /"b"/);
});

test('a recorded session keeps its answers in the order asked and its verdicts on decided approvals', () => {
  const q = (question: string, askedAt: number, choice: string | null) => ({ question, options: [], evidence: null, askedAt, answer: choice === null ? null : { choice, by: 'x', at: 0 }, closed: false });
  const a = (requestedAt: number, verdict: { decision: string; reason: string | null } | null, withdrawn = false) => ({ tool: 'propose', args: {}, requestedAt, verdict: verdict && { ...verdict, by: 'x', at: 0 }, executing: false, done: false, withdrawn: withdrawn ? { at: 0, reason: 'superseded', supersededBy: null } : null });
  const docs = {
    'quack.questions': { items: { b: q('second', 2, 'no'), a: q('first', 1, 'yes'), c: q('open', 3, null) } },
    'quack.approvals': { items: { y: a(2, { decision: 'reject', reason: 'Say why.' }), x: a(1, { decision: 'approve', reason: null }), z: a(3, null, true), w: a(4, null) } },
  };
  const record = { inputs: ['go', 'more'], outcome: { status: 'pending' }, turns: 0, usage: { input: 0, output: 0, cacheRead: 0 }, artifacts: {}, docs, runs: [] } as unknown as SessionRecord;
  assert.deepEqual((itemFrom(record, { agent: 'c', served: new Set() }).input as { asks: Policy }).asks, { answers: ['yes', 'no'], verdicts: ['approve', { reject: 'Say why.' }] });
});

test('one item\'s arms never run at once, whatever the concurrency', async () => {
  for (const concurrency of [1, 2]) {
    const live = new Set<string>();
    const overlaps: string[] = [];
    const { results } = await eachArm(['i1', 'i2', 'i3'], ['base', 'cand'], concurrency, async ({ item, arm }) => {
      if ([...live].some((l) => l.startsWith(item) || concurrency === 1)) overlaps.push(`${item} ${arm} with ${[...live]}`);
      live.add(`${item} ${arm}`);
      await new Promise((r) => setImmediate(r));
      live.delete(`${item} ${arm}`);
      return `${item} ${arm}`;
    });
    assert.deepEqual(overlaps, [], `concurrency ${concurrency}`);
    assert.equal(results.length, 6);
  }
});

const ROOT = fileURLToPath(new URL('../../../', import.meta.url));

test('an arm at a ref replays on that ref\'s agent bundle and guides over this tree\'s runtime', async () => {
  const show = (path: string) => execFileSync('git', ['show', `HEAD:${path}`], { cwd: ROOT, encoding: 'utf8' });
  const c = candidateAt('HEAD', '/nonexistent/deployment.json', join(ROOT, 'agents/contracts/agent.json'), [join(ROOT, 'packages/contracts/skills')]);
  assert.equal(readFileSync(c.agent, 'utf8'), show('agents/contracts/agent.json'));
  assert.equal(readFileSync(join(c.runtime!, '../../../contracts/skills/rest.md'), 'utf8'), show('packages/contracts/skills/rest.md'));
  // The copied runtime loads far enough to read its deployment, so its modules resolve inside the copy.
  const r = await replay({ id: 'x', input: { inputs: ['go'], pins: {} } }, c, []);
  assert.match(r.error ?? '', /nonexistent\/deployment\.json/);
  assert.throws(() => candidateAt('HEAD', 'd', 'agents/contracts/agent.json', [join(ROOT, 'agents')]), /guides must be directories under packages\//);
  assert.deepEqual(candidateAt('.', 'd', 'a', ['packages/x']), { deployment: 'd', agent: 'a' });
});

test('a run with a local items file needs no Langfuse: each arm prints its result, writes it to --out, and gets its own summary', async (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'quack-eval-local-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  writeFileSync(join(dir, 'items.json'), JSON.stringify([{ id: 'x', input: { inputs: ['go'], pins: {} } }]));
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('LANGFUSE_')));
  const main = join(ROOT, 'packages/eval/src/main.ts');
  // Piped and read to the end, never inherited, and in its own process group so the test ends it and its runtimes whatever happens.
  const child = spawn(process.execPath, [main, 'run', join(dir, 'items.json'), 'ab', '/nonexistent/deployment.json', '/nonexistent/agent.json', '--arm', 'base=.', '--arm', 'cand=.', '--out', join(dir, 'out.jsonl')], { cwd: ROOT, env, stdio: ['ignore', 'pipe', 'pipe'], detached: true });
  t.after(() => { try { process.kill(-child.pid!, 'SIGKILL'); } catch { /* already gone */ } });
  const r = { stdout: '', stderr: '', status: null as number | null };
  child.stdout!.on('data', (d) => (r.stdout += d));
  child.stderr!.on('data', (d) => (r.stderr += d));
  [r.status] = (await once(child, 'close')) as [number | null];
  assert.equal(r.status, 0, r.stderr);
  const lines = r.stdout.trim().split('\n');
  assert.match(lines[1], /^x base \{"checker_score":0,.*\} error: runtime exited with 1/);
  assert.match(lines[2], /^x cand /);
  assert.deepEqual(lines.slice(3).map((l) => l.split(':')[0]), ['ab-base', 'ab-cand']);
  assert.deepEqual(readFileSync(join(dir, 'out.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l).arm), ['base', 'cand']);
});

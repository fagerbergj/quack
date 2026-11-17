import type { ChildProcess } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';
import { fauxAssistantMessage, fauxProvider, fauxToolCall, type MutableModels } from '@earendil-works/pi-ai';
import { defineExtension, type ToolRegistration } from '@earendil-works/pi-durable';
import type { Approval, OpenQuestion } from '@quack/sdk';
import { openSession, PRINCIPAL_HEADER, principalHeader } from '@quack/session';

export type Invocation = { input: string; requestId: string };
/** A runtime's server and the launch token it answers. */
export type Runtime = { url: string; token?: string };
export type Settled = { status: 'done' | 'unanswered'; answer?: string; reason?: string; detail?: string };

const POLL_MS = 5_000;
const KILL_AFTER_MS = 10_000;

// A dropped connection is retried (undefined); any non-2xx answer is the harness failing, never the run's outcome.
async function invokeOnce({ url, token }: Runtime, invocation: Invocation, signal: AbortSignal): Promise<{ status: string } | undefined> {
  let res: Response;
  try {
    res = await fetch(`${url}/invocations`, { method: 'POST', headers: token ? { authorization: `Bearer ${token}` } : {}, body: JSON.stringify(invocation), signal });
  } catch (err) {
    if (signal.aborted) throw err;
    return undefined;
  }
  const body = (await res.json().catch(() => null)) as { status: string; error?: string; detail?: string } | null;
  if (!res.ok) throw new Error(`invocations answered HTTP ${res.status}${body?.error ?? body?.detail ? `: ${body?.error ?? body?.detail}` : ''}`);
  if (!body || typeof body.status !== 'string') throw new Error(`invocations answered HTTP ${res.status} without a status`);
  return body;
}

/** A scripted verdict on an approval: approve, reject with this reason, or end the replay there. */
export type Verdict = 'approve' | 'stop' | { reject: string };
/** How a replay answers asks: each question takes the next of `answers` when it fits, else its suggested option; each approval takes the next verdict, and one past them stops. */
export type Policy = { answers?: string[]; verdicts?: Verdict[] };
/** One ask the replay answered, in order; `answer` is the choice, `approve`, `reject: <reason>` or `stop`. */
export type Answered = { ask: 'question' | 'approval'; about: string; answer: string };
type Asks = { answers: string[]; verdicts: Verdict[]; log: Answered[] };

const PRINCIPAL = principalHeader({ kind: 'person', name: 'eval' });

// The answer as the session takes it: a recorded one among the options (or as text where text is taken), else the suggested option.
function answerFor(q: OpenQuestion, recorded: string | undefined): { choice: string } | { text: string } {
  if (recorded !== undefined && q.options.includes(recorded)) return { choice: recorded };
  if (recorded && q.freeText) return { text: recorded };
  const choice = q.options[q.suggested ?? 0];
  if (choice === undefined) throw new Error(`question "${q.question}" has no option to answer with`);
  return { choice };
}

// A 404 or 409 is an ask that closed or was withdrawn since it was listed, so it needs no answer.
async function call({ url, token }: Runtime, path: string, signal: AbortSignal, body?: unknown): Promise<any> {
  const res = await fetch(`${url}${path}`, { method: body ? 'POST' : 'GET', headers: { ...(token && { authorization: `Bearer ${token}` }), [PRINCIPAL_HEADER]: PRINCIPAL }, body: body === undefined ? undefined : JSON.stringify(body), signal });
  if (res.status === 404 || res.status === 409) return undefined;
  if (!res.ok) throw new Error(`${path} answered HTTP ${res.status}: ${(await res.text()).slice(0, 300)}`);
  return res.json();
}

// Answers the open questions, then decides the pending approvals; true when an approval no verdict covers ends the replay.
async function answerAsks(runtime: Runtime, asks: Asks, signal: AbortSignal): Promise<boolean> {
  const { questions } = (await call(runtime, '/questions', signal)) as { questions: OpenQuestion[] };
  for (const q of questions.sort((a, b) => a.askedAt - b.askedAt)) {
    const body = answerFor(q, asks.answers.shift());
    if (await call(runtime, `/questions/${encodeURIComponent(q.id)}`, signal, body)) asks.log.push({ ask: 'question', about: q.question, answer: 'choice' in body ? body.choice : body.text });
  }
  const { approvals } = (await call(runtime, '/approvals', signal)) as { approvals: Array<Approval & { id: string }> };
  for (const a of approvals.sort((x, y) => x.requestedAt - y.requestedAt)) {
    const v = asks.verdicts.shift() ?? 'stop';
    if (v === 'stop') return (asks.log.push({ ask: 'approval', about: a.tool, answer: 'stop' }), true);
    const decision = v === 'approve' ? { decision: 'approve' } : { decision: 'reject', reason: v.reject };
    if (await call(runtime, `/approvals/${encodeURIComponent(a.id)}`, signal, decision)) asks.log.push({ ask: 'approval', about: a.tool, answer: v === 'approve' ? v : `reject: ${v.reject}` });
    else asks.verdicts.unshift(v);
  }
  return false;
}

// Polls one invocation by its request id until the run settles: dedupe answers a repeat with the run's status, so no
// request outlives fetch's header timeout. A runtime exit or the deadline ends the wait at once; `asks` answers between polls.
export async function untilSettled(runtime: Runtime, invocation: Invocation, deadline: number, child?: ChildProcess, pollMs = POLL_MS, asks?: Asks): Promise<Settled | 'stopped'> {
  const stop = new AbortController();
  const exited = () => stop.abort(new Error(`runtime exited with ${child?.exitCode ?? child?.signalCode}`));
  child?.once('exit', exited);
  if (child && (child.exitCode !== null || child.signalCode)) exited();
  const timer = setTimeout(() => stop.abort(new Error('run timed out')), Math.max(0, deadline - Date.now()));
  try {
    for (;;) {
      const r = await invokeOnce(runtime, invocation, stop.signal);
      if (r?.status === 'done' || r?.status === 'unanswered') return r as Settled;
      if (r && asks && (await answerAsks(runtime, asks, stop.signal))) return 'stopped';
      await sleep(pollMs, undefined, { signal: stop.signal });
    }
  } catch (err) {
    throw stop.signal.aborted ? stop.signal.reason : err;
  } finally {
    clearTimeout(timer);
    child?.off('exit', exited);
  }
}

/** Submits each input once the run before it settles, as the recording's later inputs followed a finished run, answering asks by `policy` into `log`; true when an approval stopped it. */
export async function drive(runtime: Runtime, inputs: string[], policy: Policy, log: Answered[], deadline: number, child?: ChildProcess, pollMs = POLL_MS): Promise<boolean> {
  const asks: Asks = { answers: [...(policy.answers ?? [])], verdicts: [...(policy.verdicts ?? [])], log };
  for (const [i, input] of inputs.entries()) {
    if ((await untilSettled(runtime, { input, requestId: `eval-${i + 1}` }, deadline, child, pollMs, asks)) === 'stopped') return true;
  }
  return false;
}

/** Stops a child: SIGTERM, then SIGKILL after `graceMs`, since a runtime's graceful close waits on its in-flight run. */
export async function stopChild(child: ChildProcess, graceMs = KILL_AFTER_MS): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise<void>((r) => child.once('exit', () => r()));
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), graceMs);
  await exited;
  clearTimeout(timer);
}

/** Runs `tool` once in a scripted one-tool session at `dir`: the judge's calls need a tool's api, so a session hosts them. */
export async function driveTool(models: MutableModels, dir: string, tool: ToolRegistration<any, any>, requestId: string): Promise<void> {
  const faux = fauxProvider({ models: [{ id: 'driver' }] });
  faux.setResponses([fauxAssistantMessage(fauxToolCall(tool.name, {}), { stopReason: 'toolUse' }), fauxAssistantMessage('done')]);
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const session = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [defineExtension({ name: tool.name, tools: [tool] })] } });
  try {
    const result = await session.submit(`Run ${tool.name}.`, requestId, true);
    if (result.status !== 'done') throw new Error(`${tool.name} run ${result.status}${'reason' in result ? `: ${result.reason}` : ''}`);
  } finally {
    await session.close();
  }
}

import type { ChildProcess } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';
import { fauxAssistantMessage, fauxProvider, fauxToolCall, type MutableModels } from '@earendil-works/pi-ai';
import { defineExtension, type ToolRegistration } from '@earendil-works/pi-durable';
import { openSession } from '@quack/session';

export type Invocation = { input: string; requestId: string };
export type Settled = { status: 'done' | 'unanswered'; answer?: string; reason?: string; detail?: string };

const POLL_MS = 5_000;
const KILL_AFTER_MS = 10_000;

// A dropped connection is retried (undefined); any non-2xx answer is the harness failing, never the run's outcome.
async function invokeOnce(url: string, invocation: Invocation, signal: AbortSignal): Promise<{ status: string } | undefined> {
  let res: Response;
  try {
    res = await fetch(`${url}/invocations`, { method: 'POST', body: JSON.stringify(invocation), signal });
  } catch (err) {
    if (signal.aborted) throw err;
    return undefined;
  }
  const body = (await res.json().catch(() => null)) as { status: string; error?: string; detail?: string } | null;
  if (!res.ok) throw new Error(`invocations answered HTTP ${res.status}${body?.error ?? body?.detail ? `: ${body?.error ?? body?.detail}` : ''}`);
  if (!body || typeof body.status !== 'string') throw new Error(`invocations answered HTTP ${res.status} without a status`);
  return body;
}

// Polls one invocation by its request id until the run settles: dedupe answers a repeat with the run's status, so no
// request outlives fetch's header timeout. A runtime exit or the deadline ends the wait at once.
export async function untilSettled(url: string, invocation: Invocation, deadline: number, child?: ChildProcess, pollMs = POLL_MS): Promise<Settled> {
  const stop = new AbortController();
  const exited = () => stop.abort(new Error(`runtime exited with ${child?.exitCode ?? child?.signalCode}`));
  child?.once('exit', exited);
  if (child && (child.exitCode !== null || child.signalCode)) exited();
  const timer = setTimeout(() => stop.abort(new Error('run timed out')), Math.max(0, deadline - Date.now()));
  try {
    for (;;) {
      const r = await invokeOnce(url, invocation, stop.signal);
      if (r?.status === 'done' || r?.status === 'unanswered') return r as Settled;
      await sleep(pollMs, undefined, { signal: stop.signal });
    }
  } catch (err) {
    throw stop.signal.aborted ? stop.signal.reason : err;
  } finally {
    clearTimeout(timer);
    child?.off('exit', exited);
  }
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

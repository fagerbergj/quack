import { submitToSession } from '@quack/sdk';
export type StartConfig = { distributorUrl: string; fetch?: typeof fetch; now: () => Date; /** Joins a retry to the first submission; each call without one is its own. */ requestId?: string };

/** On-demand start: submits the job to the distributor under the session key; each call is its own submission. */
export async function startJob(config: StartConfig, agent: string, key: string, input: unknown) {
  const res = await submitToSession(config.distributorUrl, { key, agent, input: JSON.stringify(input), requestId: config.requestId ?? `start:${key}:${config.now().toISOString()}` }, config.fetch);
  return { ok: res.ok, status: res.status, sessionKey: key };
}

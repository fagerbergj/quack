import { submitToSession } from '@quack/sdk';
export type StartConfig = { distributorUrl: string; fetch?: typeof fetch; now: () => Date };

/** On-demand start (SL-2): submits the job to the distributor under the session key; each call is its own submission. */
export async function startJob(config: StartConfig, agent: string, key: string, input: unknown) {
  const res = await submitToSession(config.distributorUrl, { key, agent, input: JSON.stringify(input), requestId: `start:${key}:${config.now().toISOString()}` }, config.fetch);
  return { ok: res.ok, status: res.status, sessionKey: key };
}

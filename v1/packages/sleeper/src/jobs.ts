export type StartConfig = { distributorUrl: string; fetch?: typeof fetch; now: () => Date };

/** On-demand start (SL-2): submits the job to the distributor under the session key; each call is its own submission. */
export async function startJob(config: StartConfig, agent: string, key: string, input: unknown) {
  const url = `${config.distributorUrl}/sessions/${encodeURIComponent(key)}/invocations?agent=${agent}`;
  const res = await (config.fetch ?? fetch)(url, { method: 'POST', body: JSON.stringify({ input: JSON.stringify(input), requestId: `start:${key}:${config.now().toISOString()}` }) });
  return { ok: res.ok, status: res.status, sessionKey: key };
}

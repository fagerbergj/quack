import { Type } from '@earendil-works/pi-ai';
import type { Action } from '@quack/sdk';

const JOBS = ['lineup', 'waivers', 'trends', 'digest', 'retro', 'trade-finder', 'history'] as const;
const params = Type.Object({
  job: Type.String({ enum: [...JOBS], description: 'Which analysis to run' }),
  league: Type.String({ minLength: 1, description: 'Sleeper league id' }),
  week: Type.Optional(Type.Integer({ minimum: 1, description: 'NFL week; the current week when omitted' })),
});

/** Runs a job through the Sleeper service's on-demand endpoint (SL-2), which owns the session key and input. */
export const runJob: Action<typeof params> = {
  name: 'run_job',
  summary: 'Run a Sleeper fantasy job (lineup, waivers, trends, digest, retro, trade-finder, history) for a league and week',
  params,
  env: ['QUACK_SLEEPER_URL', 'QUACK_SLEEPER_TOKEN'],
  async start({ job, league, week }, ctx) {
    const { QUACK_SLEEPER_URL: url, QUACK_SLEEPER_TOKEN: token } = ctx.env;
    if (!url || !token) throw new Error('the Sleeper service is not configured for this agent');
    const res = await ctx.fetch(`${url}/leagues/${encodeURIComponent(league)}/jobs/${job}${week ? `?week=${week}` : ''}`, { method: 'POST', headers: { authorization: `Bearer ${token}` } });
    const body = (await res.json().catch(() => ({}))) as { sessionKey?: string; error?: string };
    if (!res.ok || !body.sessionKey) throw new Error(body.error ?? `the Sleeper service refused the job: HTTP ${res.status}`);
    return { session: body.sessionKey, title: `Sleeper ${job}, league ${league}${week ? `, week ${week}` : ''}` };
  },
};

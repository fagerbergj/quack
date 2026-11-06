import { int, obj, oneOf, opt, str } from '@earendil-works/pi-schema';
import { appEndpoint, type Action } from '@quack/sdk';

const JOBS = ['lineup', 'waivers', 'trends', 'digest', 'retro', 'trade-finder', 'history'] as const;
const params = obj({
  job: oneOf(JOBS, { description: 'Which analysis to run' }),
  league: opt(str({ minLength: 1, description: 'Sleeper league id; the configured league when omitted' })),
  week: opt(int({ minimum: 1, description: 'NFL week; the current week when omitted' })),
}, { open: true });

/** Runs a job through the Sleeper app's on-demand endpoint, which owns the session key and input. */
export const runJob: Action<typeof params> = {
  name: 'run_job',
  summary: 'Run a Sleeper fantasy job (lineup, waivers, trends, digest, retro, trade-finder, history) for a week, in the configured league unless one is named',
  params,
  env: ['QUACK_INDEX_URL', 'QUACK_INDEX_TOKEN'],
  async start({ job, league, week }, ctx) {
    const { url, token } = appEndpoint('sleeper', ctx.env);
    // The page's route runs a job in the service's configured league, so the model need not know its id.
    const path = league ? `/leagues/${encodeURIComponent(league)}/jobs/${job}` : `/${job}`;
    const res = await ctx.fetch(`${url}${path}${week ? `?week=${week}` : ''}`, { method: 'POST', headers: { authorization: `Bearer ${token}` } });
    const body = (await res.json().catch(() => ({}))) as { sessionKey?: string; error?: string };
    if (!res.ok || !body.sessionKey) throw new Error(body.error ?? `the Sleeper service refused the job: HTTP ${res.status}`);
    return { session: body.sessionKey, title: `Sleeper ${job}${league ? `, league ${league}` : ''}${week ? `, week ${week}` : ''}` };
  },
};

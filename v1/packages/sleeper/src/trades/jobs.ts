import type { SleeperClient } from '../client.ts';
import { startJob, type StartConfig } from '../jobs.ts';
import { etDate } from '../lineup/jobs.ts';
import type { ScheduleConfig } from '../schedules.ts';
import { nextProcessing } from '../waivers/jobs.ts';
import type { FinderInput, TradeInput } from './facts.ts';

export const FINDER_AGENT = 'sleeper-trade-finder';
export const TRADE_AGENT = 'sleeper-trade';
/** Waivers settle by now after they process, so the finder sees the rosters the claims produced. */
const SETTLE_MS = 2 * 60 * 60_000;
const WEEK_MS = 7 * 24 * 60 * 60_000;

/** Reruns within a week continue one session. */
export const finderKey = (league: string, week: number) => `sleeper:${league}:${week}:trade-finder`;
/** A trade talk keys on the trade itself, so asking about it again, or countering, continues the same session. */
export const tradeKey = (league: string, week: number, t: Pick<TradeInput, 'partner' | 'give' | 'get'>) => `sleeper:${league}:${week}:trade:${t.partner}:${[...t.give].sort().join('+')}:${[...t.get].sort().join('+')}`;

/** The weekly run: once the league's next waiver processing has settled, useful until the same time a week later; null when the league sets no processing time. */
export function finderRun(now: number, settings: Parameters<typeof nextProcessing>[1] | undefined) {
  const processing = settings && nextProcessing(now, settings);
  if (!processing) return null;
  const at = processing + SETTLE_MS;
  return { at, deadline: at + WEEK_MS };
}

/** Registers the next trade-finder run; the id carries the run date, so a re-registration after the week turns replaces it with the new week. */
export async function registerFinderSchedules(config: ScheduleConfig, client: SleeperClient, league: string, owner: string, now: () => number = Date.now): Promise<string[]> {
  const state = await client.state();
  if (state.season_type !== 'regular') return [];
  const run = finderRun(now(), (await client.league(league)).settings);
  if (!run) return [];
  const id = `sleeper/trade-finder/${league}/${etDate(run.at)}`;
  const input: FinderInput = { league, week: state.week, owner };
  const res = await (config.fetch ?? fetch)(`${config.url}/schedules`, {
    method: 'POST',
    headers: { authorization: `Bearer ${config.token}` },
    body: JSON.stringify({ id, agent: FINDER_AGENT, times: [new Date(run.at).toISOString()], deadline: new Date(run.deadline).toISOString(), sessionKey: finderKey(league, state.week), input: JSON.stringify(input) }),
  });
  if (!res.ok) throw new Error(`schedules: registering ${id} returned ${res.status}`);
  return [id];
}

/** On-demand start (SL-2): each call is its own submission under the week's session key. */
export const startFinder = (config: StartConfig, input: FinderInput) => startJob(config, FINDER_AGENT, finderKey(input.league, input.week), input);
export const startTrade = (config: StartConfig, input: TradeInput) => startJob(config, TRADE_AGENT, tradeKey(input.league, input.week, input), input);

/** `?partner=<roster id>&give=1,2&get=3`: the offer, from your side, as the on-demand route carries it. */
export function tradeParams(params: URLSearchParams): Pick<TradeInput, 'partner' | 'give' | 'get'> | string {
  const list = (name: string) => (params.get(name) ?? '').split(',').filter(Boolean);
  const partner = params.get('partner');
  if (!partner || !list('give').length || !list('get').length) return 'a trade needs partner, give and get';
  return { partner, give: list('give'), get: list('get') };
}

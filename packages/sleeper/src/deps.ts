import { appEndpoint } from '@quack/sdk';
import { createSleeperClient } from './client.ts';
import { exaNews } from './lineup/news.ts';
import type { LineupDeps } from './lineup/workflow.ts';
import type { NewsSearch, SleeperClient } from './ports.ts';

type Env = Record<string, string | undefined>;

/** A GET on the Sleeper app (QUACK_INDEX_URL, QUACK_INDEX_TOKEN), read when called. */
export function appGet(env: Env) {
  const { url, token } = appEndpoint('sleeper', env);
  return async (path: string) => {
    const res = await fetch(`${url}${path}`, { headers: { authorization: `Bearer ${token}` } });
    if (!res.ok) throw new Error(`sleeper service: ${path} returned ${res.status}`);
    return res.json();
  };
}

const clients = new Map<string, SleeperClient>();

/** One client per session process, whose players dump is the app's day-long copy: Sleeper asks for that fetch at most daily. */
export function sessionClient(env: Env = process.env): SleeperClient {
  const { url, token } = appEndpoint('sleeper', env);
  const key = `${url}\n${token}`;
  const get = appGet(env);
  if (!clients.has(key)) clients.set(key, createSleeperClient({ players: () => get('/players') }));
  return clients.get(key)!;
}

const news = new Map<string, NewsSearch>();

/** One Exa adapter per key, so its concurrency bound and run cache span every job in the process; absent without QUACK_EXA_API_KEY. */
export function sessionNews(env: Env = process.env): NewsSearch | undefined {
  const apiKey = env.QUACK_EXA_API_KEY;
  if (!apiKey) return undefined;
  const key = `${env.QUACK_EXA_URL ?? ''}\n${apiKey}`;
  if (!news.has(key)) news.set(key, exaNews({ apiKey, url: env.QUACK_EXA_URL }));
  return news.get(key);
}

/** A job's wiring from the environment: the Sleeper API, with the players dump from the Sleeper app, and, when QUACK_EXA_API_KEY is set, news. */
export function depsFromEnv(env: Env = process.env): LineupDeps {
  // Without the Exa key the news checks are simply not scored, and the verdict says so.
  return { client: sessionClient(env), news: sessionNews(env) };
}

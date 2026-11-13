import type { ToolExecutionApi } from '@earendil-works/pi-durable';
import { limiter, type DecisionModelRef } from '@quack/judge';
import type { NewsSearch, Searchable, SubjectCheck } from '../ports.ts';
import type { NewsItem } from './facts.ts';

export type { Searchable, SubjectCheck } from '../ports.ts';

export const NEWS_WINDOW_MS = 72 * 3_600_000;
const PER_PLAYER = 3;
/** Searches in flight per process; a job's searches queue behind it rather than being dropped. */
export const EXA_CONCURRENCY = 4;
/** Results kept for repeat searches (same player, same cutoff); the oldest go first. */
const CACHE_ENTRIES = 2000;

// Credible outlets only (SL news tiers: league, team, national desks); any other host is dropped, content farms included.
export const NEWS_DOMAINS = [
  'nfl.com', 'espn.com', 'nbcsports.com', 'cbssports.com', 'sports.yahoo.com', 'apnews.com', 'theathletic.com', 'fantasypros.com',
  'buffalobills.com', 'miamidolphins.com', 'patriots.com', 'newyorkjets.com', 'baltimoreravens.com', 'bengals.com', 'clevelandbrowns.com', 'steelers.com',
  'houstontexans.com', 'colts.com', 'jaguars.com', 'titansonline.com', 'denverbroncos.com', 'chiefs.com', 'raiders.com', 'chargers.com',
  'dallascowboys.com', 'giants.com', 'philadelphiaeagles.com', 'commanders.com', 'chicagobears.com', 'detroitlions.com', 'packers.com', 'vikings.com',
  'atlantafalcons.com', 'panthers.com', 'neworleanssaints.com', 'buccaneers.com', 'azcardinals.com', 'therams.com', '49ers.com', 'seahawks.com',
];

export function credible(url: string): boolean {
  try {
    const host = new URL(url).hostname.toLowerCase();
    return NEWS_DOMAINS.some((d) => host === d || host.endsWith(`.${d}`));
  } catch {
    return false;
  }
}

type Hit = { title?: string; url: string; publishedDate?: string; highlights?: Array<string | { text?: string }> };

const EXCERPT_CHARS = 280;
const mentions = (text: string | undefined, name: string) => (text ?? '').toLowerCase().includes(name.toLowerCase());
// Exa highlights run to several paragraphs, so the excerpt is a window on the name, not the highlight's start.
const around = (text: string, name: string) => {
  const flat = text.replace(/\s+/g, ' ');
  const start = Math.max(0, flat.toLowerCase().indexOf(name.toLowerCase()) - EXCERPT_CHARS / 2);
  return flat.slice(start, start + EXCERPT_CHARS).trim();
};
const highlights = (h: Hit) => (h.highlights ?? []).map((x) => (typeof x === 'string' ? x : (x.text ?? '')));

/** Exa matches by topic, so an item about a teammate or a team report can come back; a candidate must name the player in full. */
function item(h: Hit, name: string): NewsItem | undefined {
  if (mentions(h.title, name)) return { title: h.title!, url: h.url, date: h.publishedDate ?? '' };
  const text = highlights(h).find((t) => mentions(t, name));
  return text === undefined ? undefined : { title: h.title || h.url, url: h.url, date: h.publishedDate ?? '', excerpt: around(text, name) };
}

/** One news item as a prompt line: the link, its date, and the excerpt that names the player when the title does not. */
export const newsLink = (n: NewsItem) => `[${n.title}](${n.url}), ${n.date.slice(0, 16)}${n.excerpt ? `: "${n.excerpt}"` : ''}`;

type ExaConfig = { apiKey: string; url?: string; fetch?: typeof fetch };

const remember = <V>(cache: Map<string, V>, key: string, value: V) => {
  if (cache.size >= CACHE_ENTRIES) cache.delete(cache.keys().next().value!);
  cache.set(key, value);
};
const fold = (s: string) => s.normalize('NFKD').replace(/\p{M}/gu, '').replace(/[’‘]/g, "'").toLowerCase();
const escaped = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const namesInTitle = (n: NewsItem, p: Searchable) => new RegExp(`(^|[^\\p{L}\\p{N}])${escaped(fold(p.name))}($|[^\\p{L}\\p{N}])`, 'u').test(fold(n.title));

/** Exa news for one player, newest first: dated items inside the window only. A failed search yields none, so one outage never fails the lineup, and is not cached. */
// A title naming the player keeps a report; any other is his only when `about` says yes, so with no answer it is dropped.
export function exaNews(config: ExaConfig): NewsSearch {
  const slot = limiter(EXA_CONCURRENCY);
  // The decision model serves one request at a time, so the checks queue rather than crowd it.
  const ask = limiter(1);
  const cache = new Map<string, Promise<NewsItem[] | null>>();
  const answers = new Map<string, boolean>();
  const isHis = async (p: Searchable, n: NewsItem, about?: SubjectCheck) => {
    if (namesInTitle(n, p)) return true;
    // A caller with no check keeps title matches only, never another caller's answers.
    if (!about) return false;
    const key = JSON.stringify([p.name, p.team, p.pos, n.url]);
    if (!answers.has(key)) {
      const yes = await ask(() => about(p, n));
      if (yes === undefined) return false;
      remember(answers, key, yes);
    }
    return answers.get(key)!;
  };
  return async (p, at, about) => {
    const key = JSON.stringify([p.name, p.team, p.pos, at]);
    if (!cache.has(key)) remember(cache, key, slot(() => searchOnce(config, p, at)));
    const found = await cache.get(key)!;
    if (found === null) cache.delete(key);
    const his = await Promise.all((found ?? []).map((n) => isHis(p, n, about)));
    return (found ?? []).filter((_, i) => his[i]).slice(0, PER_PLAYER);
  };
}

const CRITERIA = { true: 'Yes, this holds.', false: 'No, it does not hold.' };
const who = (p: Searchable) => (p.pos === 'DEF' ? `the ${p.name} defense` : `${p.name} (${p.team ?? 'no team'} ${p.pos})`);
const subjectQuestion = (p: Searchable) =>
  `Does this report give news about ${who(p)} himself, either as its subject or as his own line in an injury or practice report? Answer no when he only appears inside news about another player, such as a teammate's injury, signing or role, or in a photo caption.`;
type Classifier = Parameters<ToolExecutionApi['models']['classify']>[0];

/** A decision model such as Clef asked whether a report is about the player; undefined when it is missing or gives no answer. */
export function clefSubject(api: ToolExecutionApi, ref: DecisionModelRef, signal?: AbortSignal): SubjectCheck {
  return async (p, n) => {
    const model = api.models.getAllModels(ref.provider).find((m) => m.id === ref.modelId && m.type === 'classifier') as Classifier | undefined;
    if (!model) return undefined;
    const state = { player: who(p), report: { title: n.title, ...(n.excerpt ? { excerpt: n.excerpt } : {}) } };
    try {
      const r = await api.models.classify(model, { state, questions: { q0: { type: 'bool', instructions: subjectQuestion(p), criteria: CRITERIA } } }, { maxRetries: 0, signal });
      const a = r.stopReason === 'stop' ? r.answers.q0 : undefined;
      return a?.type === 'bool' && a.probability !== undefined ? a.probability >= 0.5 : undefined;
    } catch {
      return undefined;
    }
  };
}

/** A job's news search with its subject check bound: the job's decision model when it names one, else the title alone decides. */
export const subjectNews = (search: NewsSearch | undefined, api: ToolExecutionApi, ref: DecisionModelRef | undefined, signal?: AbortSignal): NewsSearch | undefined =>
  search && ((p, at) => search(p, at, ref && clefSubject(api, ref, signal)));

async function searchOnce(config: ExaConfig, p: Searchable, at: number): Promise<NewsItem[] | null> {
  const since = at - NEWS_WINDOW_MS;
  try {
    const res = await (config.fetch ?? fetch)(`${config.url ?? 'https://api.exa.ai'}/search`, {
      method: 'POST',
      headers: { 'x-api-key': config.apiKey, 'content-type': 'application/json' },
      body: JSON.stringify({ query: `${p.name} ${p.team ?? ''} ${p.pos} injury status news`, numResults: PER_PLAYER, category: 'news', contents: { highlights: true }, includeDomains: NEWS_DOMAINS, startPublishedDate: new Date(since).toISOString() }),
      signal: AbortSignal.timeout(20_000),
    });
    if (!res.ok) return null;
    const hits = ((await res.json()) as { results?: Hit[] }).results ?? [];
    // Exa's date filter is advisory, so the window and the allowlist are enforced here too.
    return hits
      .filter((h) => credible(h.url))
      .flatMap((h) => item(h, p.name) ?? [])
      .filter((n) => Date.parse(n.date) >= since && Date.parse(n.date) <= at)
      .sort((a, b) => Date.parse(b.date) - Date.parse(a.date));
  } catch {
    return null;
  }
}

/** News for each player, searched together under the adapter's bound; no source means no links, not an error. */
export async function newsFor(search: NewsSearch | undefined, players: Array<Searchable & { id: string }>, at: number): Promise<Record<string, NewsItem[]>> {
  if (!search) return {};
  const unique = [...new Map(players.map((p) => [p.id, p])).values()];
  return Object.fromEntries(await Promise.all(unique.map(async (p) => [p.id, await search(p, at)] as const)));
}

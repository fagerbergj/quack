import { limiter } from '../limit.ts';
import type { NewsSearch, Searchable } from '../ports.ts';
import type { NewsItem } from './facts.ts';

export type { Searchable } from '../ports.ts';

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

/** Exa matches by topic, so an item about a teammate or a team report can come back; keep one only if it names the player in full. */
function item(h: Hit, name: string): NewsItem | undefined {
  if (mentions(h.title, name)) return { title: h.title!, url: h.url, date: h.publishedDate ?? '' };
  const text = highlights(h).find((t) => mentions(t, name));
  return text === undefined ? undefined : { title: h.title || h.url, url: h.url, date: h.publishedDate ?? '', excerpt: around(text, name) };
}

/** One news item as a prompt line: the link, its date, and the excerpt that names the player when the title does not. */
export const newsLink = (n: NewsItem) => `[${n.title}](${n.url}), ${n.date.slice(0, 16)}${n.excerpt ? `: "${n.excerpt}"` : ''}`;

type ExaConfig = { apiKey: string; url?: string; fetch?: typeof fetch };

/** Exa news for one player, newest first: dated items inside the window only. A failed search yields none, so one outage never fails the lineup, and is not cached. */
export function exaNews(config: ExaConfig): NewsSearch {
  const slot = limiter(EXA_CONCURRENCY);
  const cache = new Map<string, Promise<NewsItem[] | null>>();
  return async (p, at) => {
    const key = JSON.stringify([p.name, p.team, p.pos, at]);
    if (!cache.has(key)) {
      if (cache.size >= CACHE_ENTRIES) cache.delete(cache.keys().next().value!);
      cache.set(key, slot(() => searchOnce(config, p, at)));
    }
    const found = await cache.get(key)!;
    if (found === null) cache.delete(key);
    return found ?? [];
  };
}

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
      .sort((a, b) => Date.parse(b.date) - Date.parse(a.date))
      .slice(0, PER_PLAYER);
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

import type { NewsItem, PlayerFact } from './facts.ts';

export const NEWS_WINDOW_MS = 72 * 3_600_000;
const PER_PLAYER = 3;

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

type Hit = { title?: string; url: string; publishedDate?: string };

/** Exa news for one player, newest first: dated items inside the window only. A failed search yields none, so one outage never fails the lineup. */
export function exaNews(config: { apiKey: string; url?: string }) {
  return async (p: PlayerFact, at: number): Promise<NewsItem[]> => {
    const since = at - NEWS_WINDOW_MS;
    try {
      const res = await fetch(`${config.url ?? 'https://api.exa.ai'}/search`, {
        method: 'POST',
        headers: { 'x-api-key': config.apiKey, 'content-type': 'application/json' },
        body: JSON.stringify({ query: `${p.name} ${p.team ?? ''} ${p.pos} injury status news`, numResults: PER_PLAYER, category: 'news', includeDomains: NEWS_DOMAINS, startPublishedDate: new Date(since).toISOString() }),
        signal: AbortSignal.timeout(20_000),
      });
      if (!res.ok) return [];
      const hits = ((await res.json()) as { results?: Hit[] }).results ?? [];
      // Exa's date filter is advisory, so the window and the allowlist are enforced here too.
      return hits
        .filter((h) => credible(h.url))
        .map((h) => ({ title: h.title || h.url, url: h.url, date: h.publishedDate ?? '' }))
        .filter((n) => Date.parse(n.date) >= since && Date.parse(n.date) <= at)
        .sort((a, b) => Date.parse(b.date) - Date.parse(a.date))
        .slice(0, PER_PLAYER);
    } catch {
      return [];
    }
  };
}

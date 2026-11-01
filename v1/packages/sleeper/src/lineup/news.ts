import type { NewsItem, PlayerFact } from './facts.ts';

export const NEWS_WINDOW_MS = 72 * 3_600_000;
const PER_PLAYER = 3;

type Hit = { title?: string; url: string; publishedDate?: string };

/** Exa news for one player, newest first: dated items inside the window only. A failed search yields none, so one outage never fails the lineup. */
export function exaNews(config: { apiKey: string; url?: string }) {
  return async (p: PlayerFact, at: number): Promise<NewsItem[]> => {
    const since = at - NEWS_WINDOW_MS;
    try {
      const res = await fetch(`${config.url ?? 'https://api.exa.ai'}/search`, {
        method: 'POST',
        headers: { 'x-api-key': config.apiKey, 'content-type': 'application/json' },
        body: JSON.stringify({ query: `${p.name} ${p.team ?? ''} ${p.pos} injury status news`, numResults: PER_PLAYER, category: 'news', startPublishedDate: new Date(since).toISOString() }),
        signal: AbortSignal.timeout(20_000),
      });
      if (!res.ok) return [];
      const hits = ((await res.json()) as { results?: Hit[] }).results ?? [];
      // Exa's date filter is advisory, so the window is enforced here too.
      return hits
        .map((h) => ({ title: h.title || h.url, url: h.url, date: h.publishedDate ?? '' }))
        .filter((n) => Date.parse(n.date) >= since && Date.parse(n.date) <= at)
        .sort((a, b) => Date.parse(b.date) - Date.parse(a.date))
        .slice(0, PER_PLAYER);
    } catch {
      return [];
    }
  };
}

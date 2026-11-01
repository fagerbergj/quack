import { etDate, etParts } from '../lineup/jobs.ts';
import type { TrendsFacts, TrendPlayer } from './facts.ts';

export const KINDS = ['injury', 'waiver', 'news', 'note'] as const;
export type Kind = (typeof KINDS)[number];
export type TrendItem = { time: string; source: string; kind: Kind; player?: Omit<TrendPlayer, 'depth'>; text: string; link?: string };
export type TrendsCard = { kind: 'trends'; league: string; week: number; date: string; since: string | null; summary: string; items: TrendItem[]; source_note: string };

/** What the model writes: judgment and prose only. A link names a fetched item, and a source the reporter and outlet behind it. */
export type TrendsDecision = { items: Array<{ kind: Kind; playerId?: string; newsUrl?: string; source?: string; text: string }>; source_note: string };

/** `2026-11-06 (Fri)`, in the league's clock. */
export const when = (ms: number) => `${etDate(ms)} (${etParts(ms).weekday})`;

function item(f: TrendsFacts, d: TrendsDecision['items'][number]): TrendItem {
  const p = d.playerId === undefined ? undefined : f.players[d.playerId];
  if (d.playerId !== undefined && !p) throw new Error(`the model named ${d.playerId}, who is not in the facts`);
  const player = p && { id: p.id, name: p.name, pos: p.pos, team: p.team, inj: p.inj };
  if (d.newsUrl === undefined) return { time: when(Date.parse(`${f.date}T12:00:00Z`)), source: 'Sleeper', kind: d.kind, ...(player && { player }), text: d.text };
  const news = p && (f.news[p.id] ?? []).find((n) => n.url === d.newsUrl);
  if (!news) throw new Error(`the model linked ${d.newsUrl}, which is not news for ${d.playerId}`);
  if (!d.source) throw new Error(`the model linked ${d.newsUrl} without naming its reporter and outlet`);
  return { time: when(Date.parse(news.date)), source: d.source, kind: d.kind, player, text: d.text, link: news.url };
}

/** Times, sources, links and players come from the facts; the model supplies the kind, the sentence and who reported it. */
export function buildTrendsCard(f: TrendsFacts, d: TrendsDecision): TrendsCard {
  const items = d.items.map((i) => item(f, i));
  const summary = f.since === null ? 'First snapshot: nothing to compare with yet.' : `${items.length} item${items.length === 1 ? '' : 's'} since ${f.since}.`;
  return { kind: 'trends', league: f.league, week: f.week, date: f.date, since: f.since, summary, items, source_note: d.source_note };
}

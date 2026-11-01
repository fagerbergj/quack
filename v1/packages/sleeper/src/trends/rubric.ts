import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import { credible } from '../lineup/news.ts';
import { KINDS, type TrendItem, type TrendsCard } from './card.ts';
import type { TrendsFacts } from './facts.ts';

const share = (n: number, of: number, detail: string): CodeResult => ({ met: of === 0 ? 1 : n / of, detail: n < of ? detail : undefined });
const parse = (artifact: string) => JSON.parse(artifact) as TrendsCard;
const factsOf = (ev: Evidence) => ev.facts[0].state as unknown as TrendsFacts;
const within = (s: unknown, max: number) => typeof s === 'string' && s.length > 0 && s.length <= max;

// The caps are the trends schema's (time 40, source 60); the text cap is the model schema's, so one sentence stays one line in the timeline.
export const TEXT_MAX = 160;

function itemShape(artifact: string): CodeResult {
  const { items } = parse(artifact);
  const bad = items.filter((i) => !within(i.time, 40) || !within(i.source, 60) || !within(i.text, TEXT_MAX) || !(KINDS as readonly string[]).includes(i.kind));
  return share(items.length - bad.length, items.length, `${bad.map((i) => i.text.slice(0, 30)).join('; ')} miss a time, source, kind or sentence within its cap`);
}

// The link must be one the news step returned for that very player, and from an allowlisted host: a link is never the model's to supply.
function linkFetched(artifact: string, ev: Evidence): CodeResult {
  const f = factsOf(ev);
  const linked = parse(artifact).items.filter((i) => i.link);
  const bad = linked.filter((i) => !i.player || !credible(i.link!) || !(f.news[i.player.id] ?? []).some((n) => n.url === i.link));
  return share(linked.length - bad.length, linked.length, `${bad.map((i) => i.link).join(', ')} were not returned by the news search for that player`);
}

/** Row units carry the item, Sleeper's designation by name and the dated news for the player, so a check sees only what it needs. */
export function trendsEvidence(f: TrendsFacts) {
  return async (_api: unknown, sealed: { content: string }): Promise<Evidence> => {
    const card = parse(sealed.content);
    const unit = (i: TrendItem, n: number) => ({
      id: `item${n + 1}`,
      state: { item: i, since: f.since, news: i.player ? (f.news[i.player.id] ?? []) : [], designations: i.player ? { [i.player.name]: i.player.inj ?? 'none' } : {} } as never,
    });
    const units = card.items.map(unit);
    return {
      facts: [{ id: 'facts', state: f as never }],
      row: units,
      // Where Sleeper holds a designation and the web has a report on the same player, the two can conflict.
      conflict: units.filter((u, n) => card.items[n].player?.inj && (f.news[card.items[n].player!.id]?.length ?? 0) > 0),
      sourced: units.filter((_, n) => card.items[n].link),
      card: [{ id: 'card', state: { summary: card.summary, source_note: card.source_note, items: card.items.map((i) => ({ player: i.player?.name, source: i.source, text: i.text })) } as never }],
    };
  };
}

// The audit's trends table (docs/v1/audits/sleeper.md), unreplayed: D45 asks for replay before any weight gates delivery.
export const trendsRubric: Rubric = {
  name: 'sleeper.trends',
  version: 1,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'item_shape', weight: 'gate', code: itemShape },
      { id: 'link_fetched', weight: 'gate', code: linkFetched },
    ] },
    { id: 'items_are_sourced', checks: [
      { id: 'conflict_stated', weight: 3, scope: 'conflict', question: 'Sleeper holds a `designations` entry for this player and `news` has dated reports. Where they disagree, does the item say so and name the rule it followed: the newer dated report from a credible outlet beats Sleeper\'s lagging designation?' },
      { id: 'original_source_named', weight: 2, scope: 'sourced', question: 'Does `source` name the reporter and the outlet, not an aggregator?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'aggregator_only', weight: -3, scope: 'sourced', question: 'Is an aggregator the only source named for this item?' },
    ] },
  ],
};

/** Checks that need dated web reports; without a news source there is nothing for them to read. */
export const NEWS_CHECKS = ['conflict_stated', 'original_source_named', 'aggregator_only'];

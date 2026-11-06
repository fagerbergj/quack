import type { CodeResult, Evidence, Rubric } from '@quack/judge';
import { factsOf as factsIn, parseCard, share } from '../rubric-kit.ts';
import { KINDS, type TrendItem, type TrendsCard } from './card.ts';
import type { TrendsFacts } from './facts.ts';

const parse = (artifact: string) => parseCard<TrendsCard>(artifact);
const factsOf = (ev: Evidence) => factsIn<TrendsFacts>(ev);
const within = (s: unknown, max: number) => typeof s === 'string' && s.length > 0 && s.length <= max;

// The caps are the trends schema's (time 40, source 60); the text cap is the model schema's, so one sentence stays one line in the timeline.
export const TEXT_MAX = 160;

function itemShape(artifact: string): CodeResult {
  const { items } = parse(artifact);
  const bad = items.filter((i) => !within(i.time, 40) || !within(i.source, 60) || !within(i.text, TEXT_MAX) || !(KINDS as readonly string[]).includes(i.kind));
  return share(items.length - bad.length, items.length, `${bad.map((i) => i.text.slice(0, 30)).join('; ')} miss a time, source, kind or sentence within its cap`);
}

// Ids key the card and the UI shows names; the digit and comma guards keep an add count from reading as an id.
function noBareIds(artifact: string, ev: Evidence): CodeResult {
  const ids = Object.keys(factsOf(ev).players);
  const { items } = parse(artifact);
  const bad = items.filter((i) => ids.some((id) => new RegExp(`(?<![\\d,.])${id}(?![\\d,])`).test(i.text)));
  return share(items.length - bad.length, items.length, `${bad.map((i) => i.text.slice(0, 30)).join('; ')} carry a raw player id in their text`);
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
      facts: [{ id: 'facts', whole: true as const, state: f as never }],
      row: units,
      // Where Sleeper holds a designation and the web has a report on the same player, the two can conflict.
      conflict: units.filter((u, n) => card.items[n].player?.inj && (f.news[card.items[n].player!.id]?.length ?? 0) > 0),
      sourced: units.filter((_, n) => card.items[n].link),
      card: [{ id: 'card', whole: true as const, state: { summary: card.summary, source_note: card.source_note, items: card.items.map((i) => ({ player: i.player?.name, source: i.source, text: i.text })) } as never }],
    };
  };
}

// The card builder takes each link from that player's fetched news, so a link is never checked here.
// Trends weights are unreplayed: replay them before any weight gates delivery.
export const trendsRubric: Rubric = {
  name: 'sleeper.trends',
  version: 2,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'item_shape', label: "A news item is malformed", weight: 'gate', code: itemShape },
      { id: 'no_bare_ids', label: "Player id written in the item text", weight: 'gate', code: noBareIds },
    ] },
    { id: 'items_are_sourced', checks: [
      { id: 'conflict_stated', label: "Hides a clash between news and injury tag", weight: 3, scope: 'conflict', question: 'Sleeper holds a `designations` entry for this player and `news` has dated reports. If they agree, answer yes. If they disagree, does the item say so and name the rule it followed: the newer dated report from a credible outlet beats Sleeper\'s lagging designation?' },
      { id: 'original_source_named', label: "Doesn't name the original source", weight: 2, scope: 'sourced', question: 'Does `source` name the original source: a reporter, outlet, team or league, not an aggregator?' },
    ] },
    { id: 'pitfalls', checks: [
      { id: 'aggregator_only', label: "Sources only an aggregator", weight: -3, scope: 'sourced', question: 'Is an aggregator the only source named for this item?' },
    ] },
  ],
};

/** Checks that need dated web reports; without a news source there is nothing for them to read. */
export const NEWS_CHECKS = ['conflict_stated', 'original_source_named', 'aggregator_only'];

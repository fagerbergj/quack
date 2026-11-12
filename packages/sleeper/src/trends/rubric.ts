import { registerSplitter, type CodeResult, type Declaration, type Evidence, type Inputs, type Rubric } from '@quack/judge';
import { metaFields, parseCard, share, SHARE, yes } from '../rubric-kit.ts';
import type { TrendsCard } from './card.ts';
import type { TrendsFacts } from './facts.ts';

const parse = (artifact: string) => parseCard<TrendsCard>(artifact);
const factsOf = (ev: Evidence) => ev.facts[0].state.facts as unknown as TrendsFacts;

// Ids key the card and the UI shows names; the digit and comma guards keep an add count from reading as an id.
function noBareIds(artifact: string, ev: Evidence): CodeResult {
  const ids = Object.keys(factsOf(ev).players);
  const { items } = parse(artifact);
  const bad = items.filter((i) => ids.some((id) => new RegExp(`(?<![\\d,.])${id}(?![\\d,])`).test(i.text)));
  return share(items.length - bad.length, items.length, `${bad.map((i) => i.text.slice(0, 30)).join('; ')} carry a raw player id in their text`);
}

/** Each item as one record: what the source check reads, and the legacy unit conflict_stated keeps (the item, Sleeper's designation by name, the player's dated news). */
function items(f: TrendsFacts, card: TrendsCard) {
  return card.items.map((i, n) => {
    const news = i.player ? (f.news[i.player.id] ?? []) : [];
    // Where Sleeper holds a designation and the web has a report on the same player, the two can conflict.
    const conflict = !!i.player?.inj && news.length > 0;
    return { id: `item${n + 1}`, source: i.source, link: i.link, conflict, legacy: { item: i, since: f.since, news, designations: i.player ? { [i.player.name]: i.player.inj ?? 'none' } : {} } };
  });
}

/** The judge's named texts: the facts the card was built on, and its items. */
export function trendsInputsOf(f: TrendsFacts, content: string): Inputs {
  return { facts: JSON.stringify(f), items: JSON.stringify(items(f, parse(content))) };
}
export const trendsInputs = (f: TrendsFacts) => async (_api: unknown, sealed: { content: string }): Promise<Inputs> => trendsInputsOf(f, sealed.content);

// The conflict check stays on the 27B with its legacy unit: the card's item, then the other fields at the top.
type Item = ReturnType<typeof items>[number];
registerSplitter('sleeper.trendRows', (inputs, o) => (JSON.parse(inputs[String(o.from)]) as Item[]).map((r) => ({ id: r.id, text: JSON.stringify(r.legacy.item), meta: { ...r.legacy, conflict: r.conflict } })));

/** How each model check of trendsRubric is asked: per item, each alone with nothing shared. A link is only ever a fetched report, so with no news source neither scope has items. */
export const trendsDeclaration: Declaration = {
  scopes: {
    facts: { split: { name: 'whole', from: 'facts' }, piece: 'facts' },
    sourced: { split: { name: 'jsonItems', from: 'items', pick: ['source', 'link'], where: [{ path: 'link', op: 'truthy' }] }, piece: 'item' },
    conflict: { split: { name: 'sleeper.trendRows', from: 'items', where: [{ path: 'conflict', op: 'truthy' }] }, piece: 'item', context: metaFields(['since', 'news', 'designations']) },
  },
  checks: {
    // Weighing the item against the dated reports is a claim against evidence, which the decision model passes unread, so the 27B decides it.
    conflict_stated: { scope: 'conflict', aggregate: SHARE, decisionModel: false, question: yes('Sleeper holds a `designations` entry for this player and `news` has dated reports. If they agree, answer yes. If they disagree, does the item say so and name the rule it followed: the newer dated report from a credible outlet beats Sleeper\'s lagging designation?') },
    original_source_named: { scope: 'sourced', aggregate: SHARE, question: yes('Does `source` name the original source: a reporter, outlet, team or league, not an aggregator?') },
  },
};

// The card builder takes each time, player and link from the facts, and the schema caps each field, so only what the model writes is checked.
// Trends weights are unreplayed: replay them before any weight gates delivery.
export const trendsRubric: Rubric = {
  name: 'sleeper.trends',
  version: 3,
  threshold: 0.8,
  criteria: [
    { id: 'faithful_to_tools', checks: [
      { id: 'no_bare_ids', label: "Player id written in the item text", weight: 'gate', code: noBareIds },
    ] },
    { id: 'items_are_sourced', checks: [
      { id: 'conflict_stated', label: "Hides a clash between news and injury tag", weight: 3 },
      { id: 'original_source_named', label: "Doesn't name the original source", weight: 2 },
    ] },
  ],
};

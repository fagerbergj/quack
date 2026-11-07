import { arr, lit, obj, opt, str, union } from '@earendil-works/pi-schema';
import { defineWorkflow } from '@quack/workflows';
import { newsLink } from '../lineup/news.ts';
import type { Json } from '../client.ts';
import type { LineupDeps } from '../lineup/workflow.ts';
import { buildTrendsCard, KINDS, when, type TrendsDecision } from './card.ts';
import { buildTrendsFacts, type SnapshotRef, type TrendsFacts } from './facts.ts';
import { TEXT_MAX, TRENDS_NEWS, trendsEvidence, trendsRubric } from './rubric.ts';
import { cardSteps } from '../job.ts';

export const TRENDS_WORKFLOW = 'sleeper.trends';

/** The lineup wiring plus the service's snapshots, which the diff reads: today's (taken if missing) and the history. */
export type TrendsDeps = LineupDeps & {
  snapshot: (league: string) => Promise<Json>;
  history: (league: string) => Promise<SnapshotRef[]>;
  snapshotOn: (league: string, date: string) => Promise<Json>;
};

const decisionSchema = obj({
  items: arr(
    obj({
      kind: union(KINDS.map((k) => lit(k))),
      playerId: opt(str()),
      newsUrl: opt(str()),
      source: opt(str({ maxLength: 60 })),
      text: str({ maxLength: TEXT_MAX }),
    }, { open: true }),
    { maxItems: 12 },
  ),
  source_note: str({ maxLength: 300 }),
}, { open: true });

export function factsPrompt(f: TrendsFacts): string {
  const base = (id: string) => `${f.players[id].name} (${f.players[id].pos ?? '-'}, ${f.players[id].team ?? '-'})`;
  const ids = Object.keys(f.players);
  // Two players can share name, position and team; an index keeps each lookup line unique.
  const who = (id: string) => (ids.filter((o) => base(o) === base(id)).length > 1 ? `${base(id)} #${ids.filter((o) => base(o) === base(id)).indexOf(id) + 1}` : base(id));
  const links = (id: string) => (f.news[id] ?? []).map((n) => `\n    - ${newsLink(n)}`).join('');
  const changes = f.changes.map((c) => `- ${who(c.playerId)}: ${c.field} ${c.from ?? 'none'} -> ${c.to ?? 'none'}${links(c.playerId)}`);
  const rising = f.trending.map((t) => `- ${who(t.playerId)}: ${t.adds} adds in 24 hours, held by no team here`);
  return [
    f.since ? `Sleeper changes between ${when(Date.parse(`${f.since}T12:00:00Z`))} and ${when(Date.parse(`${f.date}T12:00:00Z`))}, week ${f.week}:\n${changes.join('\n') || 'none'}` : 'This is the first snapshot, so there is nothing to compare with.',
    `Free agents rising on Sleeper:\n${rising.join('\n') || 'none'}`,
    `Values for \`playerId\`:\n${ids.map((id) => `- ${who(id)}: ${id}`).join('\n') || 'none'}`,
    'Write one item per change worth a reader\'s attention. Set `playerId` from the values above and name the player in `text`, never an id. A news link goes in `newsUrl` with `source` naming the outlet, and the reporter when known; use only the links above. When a dated report conflicts with Sleeper\'s designation, the item says so and which one you followed. `source_note` states each conflict and the rule followed, or "No source conflicts."',
  ].join('\n\n');
}

/** Trends job: facts, one model step for prose, a sealed card, then the judge's verdict on it. */
export function trendsWorkflow(deps: TrendsDeps) {
  return defineWorkflow({
    name: TRENDS_WORKFLOW,
    steps: cardSteps<TrendsFacts, TrendsDecision>(deps, {
      facts: async (_c, i, at) => buildTrendsFacts(deps, await deps.snapshot(i.league), i, at),
      schema: decisionSchema, prompt: factsPrompt, card: buildTrendsCard, rubric: trendsRubric, news: TRENDS_NEWS, evidence: trendsEvidence,
    }),
  });
}

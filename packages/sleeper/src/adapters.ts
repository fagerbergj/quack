import type { Evidence, Rubric } from '@quack/judge';
import type { EvalAdapter, RecordedSession } from '@quack/sdk';
import { historyEvidence, historyRubric } from './history/rubric.ts';
import { cardsKind } from './lineup/card.ts';
import type { Facts } from './lineup/facts.ts';
import { lineupDeclaration, lineupInputsOf, lineupRubric } from './lineup/rubric.ts';
import { DIGEST_NEWS, digestEvidence, digestRubric, RETRO_NEWS, retroEvidence, retroRubric } from './report/rubric.ts';
import { newsGated, type NewsGate } from './rubric-kit.ts';
import { TRADE_NEWS, tradeEvidence, tradeRubric } from './trades/rubric.ts';
import { TRENDS_NEWS, trendsEvidence, trendsRubric } from './trends/rubric.ts';
import type { WaiverFacts } from './waivers/facts.ts';
import { waiversDeclaration, waiversInputsOf, waiversRubric } from './waivers/rubric.ts';
import { WAIVERS_FACTS_KIND } from './waivers/workflow.ts';

type Build = (facts: never) => (api: unknown, sealed: { content: string }) => Promise<Evidence>;

// The live job gates its news checks on whether a news source is wired, and its verdict notes say when it was not; the replay gates on that note.
function hadNews(session: RecordedSession, content: string, rubric: Rubric, gate: NewsGate): boolean {
  const verdict = session.artifacts[cardsKind.kind]?.findLast((v) => v.content === content)?.verdict as { notes?: string[] } | null | undefined;
  if (!verdict) throw new Error('the card has no recorded verdict, so whether its job had a news source is unknown');
  return !(verdict.notes ?? []).includes(newsGated(rubric, gate, false).notes[0]);
}

// Every job seals onto sleeper.cards, so an adapter judges only the card kind it names, on the facts of the run that sealed it.
// A job with news checks gets one adapter per gating, each with the rubric newsGated gives the live judge; the recording picks one.
function adaptersFor(card: string, rubric: Rubric, evidence: Build, gate?: NewsGate): EvalAdapter[] {
  const variants = gate ? [true, false].map((news) => ({ news, rubric: newsGated(rubric, gate, news).rubric })) : [{ news: true, rubric }];
  return variants.map((v) => ({
    kind: cardsKind.kind,
    judge: {
      rubric: v.rubric,
      async evidence(session, content) {
        if (JSON.parse(content).kind !== card) return undefined;
        if (gate && hadNews(session, content, rubric, gate) !== v.news) return undefined;
        return evidence(recordedFacts(session, content, card) as never)(null, { content });
      },
    },
  }));
}

/** The facts of the run that sealed a recorded card of this kind; throws, declining the recording, for another kind or a run that kept none. */
function recordedFacts(session: RecordedSession, content: string, card: string) {
  if (JSON.parse(content).kind !== card) throw new Error(`not a ${card} card`);
  const facts = session.results?.facts;
  if (!facts) throw new Error(`the recording keeps no facts for this ${card} card: no workflow run that sealed it left step results`);
  return facts;
}

const lineupAdapter: EvalAdapter = {
  kind: cardsKind.kind,
  judge: { rubric: lineupRubric, declaration: lineupDeclaration, inputs: async (session, content) => lineupInputsOf(recordedFacts(session, content, 'lineup') as unknown as Facts, content) },
};

// The waivers facts pin only keeps a replay of an older dataset from refusing the recorded doc; the workflow rebuilds the facts live.
const waiversAdapter: EvalAdapter = {
  kind: cardsKind.kind,
  judge: { rubric: waiversRubric, declaration: waiversDeclaration, inputs: async (session, content) => waiversInputsOf(recordedFacts(session, content, 'waivers') as unknown as WaiverFacts, content) },
  services: { [WAIVERS_FACTS_KIND]: async () => ({ env: {}, captured: [], close: async () => {} }) },
};

/** The adapters of every judged card; waivers comes first, as the rubric an agent shows. */
export const cardAdapters: EvalAdapter[] = [
  waiversAdapter,
  lineupAdapter,
  ...adaptersFor('trade-finder', tradeRubric, tradeEvidence, TRADE_NEWS),
  ...adaptersFor('trends', trendsRubric, trendsEvidence, TRENDS_NEWS),
  ...adaptersFor('history', historyRubric, historyEvidence),
  ...adaptersFor('digest', digestRubric, digestEvidence, DIGEST_NEWS),
  ...adaptersFor('retro', retroRubric, retroEvidence, RETRO_NEWS),
];

import type { Evidence, Rubric } from '@quack/judge';
import type { EvalAdapter, RecordedSession } from '@quack/sdk';
import { historyEvidence, historyRubric } from './history/rubric.ts';
import { cardsKind } from './lineup/card.ts';
import type { Facts } from './lineup/facts.ts';
import { lineupDeclaration, lineupInputsOf, lineupRubric } from './lineup/rubric.ts';
import type { DigestFacts } from './report/digest.ts';
import type { RetroFacts } from './report/retro.ts';
import { digestDeclaration, digestInputsOf, digestRubric, retroDeclaration, retroInputsOf, retroRubric } from './report/rubric.ts';
import type { TradeFacts } from './trades/facts.ts';
import { tradeDeclaration, tradeInputsOf, tradeRubric } from './trades/rubric.ts';
import type { TrendsFacts } from './trends/facts.ts';
import { trendsDeclaration, trendsInputsOf, trendsRubric } from './trends/rubric.ts';
import type { WaiverFacts } from './waivers/facts.ts';
import { waiversDeclaration, waiversInputsOf, waiversRubric } from './waivers/rubric.ts';
import { WAIVERS_FACTS_KIND } from './waivers/workflow.ts';

type Build = (facts: never) => (api: unknown, sealed: { content: string }) => Promise<Evidence>;

// Every job seals onto sleeper.cards, so an adapter judges only the card kind it names, on the facts of the run that sealed it.
function adapterFor(card: string, rubric: Rubric, evidence: Build): EvalAdapter {
  return {
    kind: cardsKind.kind,
    judge: {
      rubric,
      async evidence(session, content) {
        if (JSON.parse(content).kind !== card) return undefined;
        return evidence(recordedFacts(session, content, card) as never)(null, { content });
      },
    },
  };
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
  { kind: cardsKind.kind, judge: { rubric: tradeRubric, declaration: tradeDeclaration, inputs: async (session, content) => tradeInputsOf(recordedFacts(session, content, 'trade-finder') as unknown as TradeFacts, content) } },
  { kind: cardsKind.kind, judge: { rubric: trendsRubric, declaration: trendsDeclaration, inputs: async (session, content) => trendsInputsOf(recordedFacts(session, content, 'trends') as unknown as TrendsFacts, content) } },
  adapterFor('history', historyRubric, historyEvidence),
  { kind: cardsKind.kind, judge: { rubric: digestRubric, declaration: digestDeclaration, inputs: async (session, content) => digestInputsOf(recordedFacts(session, content, 'digest') as unknown as DigestFacts, content) } },
  { kind: cardsKind.kind, judge: { rubric: retroRubric, declaration: retroDeclaration, inputs: async (session, content) => retroInputsOf(recordedFacts(session, content, 'retro') as unknown as RetroFacts, content) } },
];

import type { Evidence, Rubric } from '@quack/judge';
import type { EvalAdapter } from '@quack/sdk';
import { historyEvidence, historyRubric } from './history/rubric.ts';
import { cardsKind } from './lineup/card.ts';
import { lineupEvidence, lineupRubric } from './lineup/rubric.ts';
import { digestEvidence, digestRubric, retroEvidence, retroRubric } from './report/rubric.ts';
import { tradeEvidence, tradeRubric } from './trades/rubric.ts';
import { trendsEvidence, trendsRubric } from './trends/rubric.ts';

type Build = (facts: never) => (api: unknown, sealed: { content: string }) => Promise<Evidence>;

// Every job seals onto sleeper.cards, so an adapter judges only the card kind it names; the facts are the run's own facts step result.
const cardAdapter = (card: string, rubric: Rubric, evidence: Build): EvalAdapter => ({
  kind: cardsKind.kind,
  judge: {
    rubric,
    async evidence(session, content) {
      if (JSON.parse(content).kind !== card) return undefined;
      const facts = session.results?.facts;
      if (!facts) throw new Error(`the recording keeps no facts for this ${card} card: its workflow run left no step results`);
      return evidence(facts as never)(null, { content });
    },
  },
});

/** The adapters of every card judged on its workflow's facts; waivers keeps its own, which reads the facts document it records. */
export const cardAdapters: EvalAdapter[] = [
  cardAdapter('lineup', lineupRubric, lineupEvidence),
  cardAdapter('trade', tradeRubric, tradeEvidence),
  cardAdapter('trade-finder', tradeRubric, tradeEvidence),
  cardAdapter('trends', trendsRubric, trendsEvidence),
  cardAdapter('history', historyRubric, historyEvidence),
  cardAdapter('digest', digestRubric, digestEvidence),
  cardAdapter('retro', retroRubric, retroEvidence),
];

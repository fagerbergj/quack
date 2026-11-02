import type { EvalAdapter } from '@quack/sdk';
import { cardsKind } from '../lineup/card.ts';
import type { WaiverFacts } from './facts.ts';
import { waiversEvidence, waiversRubric } from './rubric.ts';
import { WAIVERS_FACTS_KIND } from './workflow.ts';

// The waivers card shares sleeper.cards with the other jobs, so the rubric applies only where the card says it is one.
export const waiversEvalAdapter: EvalAdapter = {
  kind: cardsKind.kind,
  judge: {
    rubric: waiversRubric,
    async evidence(session, content) {
      const facts = session.docs[WAIVERS_FACTS_KIND]?.facts;
      if (!facts || JSON.parse(content).kind !== 'waivers') return undefined;
      return waiversEvidence(facts as unknown as WaiverFacts)(null, { content });
    },
  },
  // The facts pin only keeps replay from refusing the recorded doc; the workflow rebuilds them live, so the stub serves nothing.
  services: { [WAIVERS_FACTS_KIND]: async () => ({ env: {}, captured: [], close: async () => {} }) },
};

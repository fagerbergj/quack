import { defineExtension, GenerationTask, hook, LiveDoc, type ConversationId } from '@earendil-works/pi-durable';

/** Continuations a run gets in a row; a cut-off after that many means the model cannot fit an action under its cap. */
export const CUT_OFF_LIMIT = 3;

/** Continues a run whose reply hit the output-token cap, which pi-durable otherwise takes as the run's final answer. */
// Counts are per run and in memory, like the loop guard's: a restart only delays the stop.
export const cutOffExtension = (limit = CUT_OFF_LIMIT) => {
  const streak = new Map<ConversationId, { run: unknown; count: number }>();
  return defineExtension({
    name: 'cut-off',
    hooks: [
      hook(GenerationTask, {
        afterResponse(message, api) {
          if (message.stopReason !== 'length') streak.delete(api.conversationId);
        },
        async onYield(answer, api, ctx) {
          if (answer.stopReason !== 'length') return undefined;
          const run = (await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs[0];
          const prior = streak.get(api.conversationId);
          const count = (prior && prior.run === run ? prior.count : 0) + 1;
          if (count > limit) return void streak.delete(api.conversationId);
          streak.set(api.conversationId, { run, count });
          return { continue: 'Your last reply hit the output limit and was cut off before you acted. Keep your reasoning short and make your next tool call now.' };
        },
      }),
    ],
  });
};

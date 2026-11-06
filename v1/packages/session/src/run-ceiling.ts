import { defineExtension, GenerationTask, hook, LiveDoc, type ConversationId } from '@earendil-works/pi-durable';
import { Failures } from '@quack/sdk';

/** Model responses one run may take; a runaway guard that fails the run, not a tuning knob. */
export const TURN_CEILING = 300;

export type CeilingSink = {
  /** Records the run's failure for "Needs you" and aborts it; `error` null clears an earlier ceiling failure. */
  record(conversation: ConversationId, run: string, error: string | null): Promise<void>;
};

/** Counts every model response of a run; the response past `limit` fails the run, loudly and once. */
// Counts are in memory like the loop guard's: a restart only delays the stop.
export const runCeilingExtension = (limit: number, sink: CeilingSink) => {
  const state = new Map<ConversationId, { run: string; turns: number }>();
  return defineExtension({
    name: 'run-ceiling',
    hooks: [
      hook(GenerationTask, {
        async afterResponse(_message, api, ctx) {
          const run = String((await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs[0] ?? '');
          let s = state.get(api.conversationId);
          if (s?.run !== run) {
            s = { run, turns: 0 };
            state.set(api.conversationId, s);
            const earlier = (await api.snapshot(Failures, api.conversationId, ctx))?.items.runaway;
            if (earlier?.error) await sink.record(api.conversationId, run, null);
          }
          if (++s.turns === limit + 1) await sink.record(api.conversationId, run, `Stopped: the run took more than ${limit} model turns without finishing, so it was ended. Read the transcript for what it kept doing, then resume or start over.`);
        },
      }),
    ],
  });
};

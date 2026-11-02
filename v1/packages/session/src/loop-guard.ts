import { createHash } from 'node:crypto';
import { contentText } from '@earendil-works/pi-ai';
import { defineExtension, hook, LiveDoc, ToolTask, type ConversationId } from '@earendil-works/pi-durable';

/** Three identical results to one call show nothing new to act on; two can be a legitimate recheck. */
export const LOOP_LIMIT = 3;

const digest = (v: unknown) => createHash('sha1').update(JSON.stringify(v)).digest('hex');

type Counts = { run: unknown; calls: Map<string, { result: string; count: number }> };

/** Ends the run when one call (name and arguments) gets the same result `limit` times, adjacent or not; a changed result restarts its count. */
// Counts are per run and in memory (a restart only delays the stop). A call at the limit keeps terminating, so a parallel round of stuck calls ends.
export const loopGuardExtension = (limit = LOOP_LIMIT) => {
  const state = new Map<ConversationId, Counts>();
  return defineExtension({
    name: 'loop-guard',
    hooks: [
      hook(ToolTask, {
        async afterTool(call, result, api, ctx) {
          const run = (await api.snapshot(LiveDoc, api.conversationId, ctx))?.run?.inputs[0];
          const prior = state.get(api.conversationId);
          const counts: Counts = prior && prior.run === run ? prior : { run, calls: new Map() };
          state.set(api.conversationId, counts);
          const key = digest([call.name, call.arguments]);
          const seen = counts.calls.get(key);
          const out = digest([result.isError ?? false, result.output ? contentText(result.output) : '']);
          const count = seen?.result === out ? Math.min(seen.count + 1, limit) : 1;
          counts.calls.set(key, { result: out, count });
          if (count < limit) return undefined;
          const reason = `Stopped: ${call.name} was called with the same arguments ${limit} times and returned the same result each time, so repeating it cannot change anything. This turn is over.`;
          return { ...result, isError: false, output: [{ type: 'text', text: reason }], control: { ...result.control, terminate: true } };
        },
      }),
    ],
  });
};

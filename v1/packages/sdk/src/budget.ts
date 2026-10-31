import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type ToolExecutionApi } from '@earendil-works/pi-durable';

/** Turns at the end of a run kept for delivering what is done; inside them a failing artifact is released (J1). */
export const FINAL_TURNS = 5;

/** The current run's turns left before its limit, written by the session's turn watchdog; null without a limit. */
export const RunBudget = defineDoc<{ turnsLeft: number | null }>({
  kind: 'quack.budget', version: 1, scope: 'conversation', history: 'latest', fork: 'initial', initial: () => ({ turnsLeft: null }), checkpointWhen: () => false,
});

export async function inFinalTurns(api: ToolExecutionApi, ctx: Context): Promise<boolean> {
  const left = (await api.snapshot(RunBudget, api.conversationId, ctx))?.turnsLeft;
  return left !== null && left !== undefined && left <= FINAL_TURNS;
}

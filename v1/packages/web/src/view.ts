import type { AssistantMessage } from '@earendil-works/pi-ai';
import type { LiveState, TranscriptItem, UsageState } from '@earendil-works/pi-durable';
import type { Spend } from './api.ts';

/** The session socket's `view` frame: the conversation view's state. `items` is absent when the transcript did not change. */
export type ViewFrame = { type: 'view'; items?: readonly TranscriptItem[]; live: LiveState; usage?: UsageState };

/** The transcript items plus what the live document says is happening now. */
export type View = {
  items: readonly TranscriptItem[];
  running: boolean;
  partial?: AssistantMessage;
  spend?: Spend;
  /** The tool call in flight. */
  tool?: { name: string; args: unknown; /** What the tool published while it ran (the judge's per-check progress). */ details?: unknown };
  /** Changes when a tool starts or ends or the run starts or ends, which is when an approval can appear or clear. */
  activity: string;
};

export const emptyView: View = { items: [], running: false, activity: '' };

type Entry = Extract<TranscriptItem, { kind: 'entry' }>['entry'];

// The usage summed over every model bucket; the index adds spend made outside the conversation.
const spendOf = (usage: UsageState | undefined): Spend => {
  const spend: Spend = { cost: 0, tokens: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } };
  for (const u of Object.values(usage?.models ?? {})) {
    spend.cost += u.cost?.total ?? 0;
    for (const k of Object.keys(spend.tokens) as Array<keyof Spend['tokens']>) spend.tokens[k] += u[k] ?? 0;
  }
  return spend;
};

// A model-issued call's arguments live in its assistant entry, not in the tool's slot.
function argsOf(items: readonly TranscriptItem[], callId: string): unknown {
  for (const item of items.toReversed()) {
    if (item.kind !== 'entry') continue;
    for (const m of (item.entry as Entry).model ?? []) {
      const call = m.role === 'assistant' && m.content.find((b) => b.type === 'toolCall' && b.id === callId);
      if (call && call.type === 'toolCall') return call.arguments;
    }
  }
  return {};
}

export function applyView(prev: View, { items = prev.items, live, usage }: ViewFrame): View {
  const slot = live.tools?.findLast((t) => t.status === 'running');
  const running = live.run !== undefined;
  const activity = JSON.stringify([running, live.tools?.map((t) => [t.callId, t.status])]);
  return { items, running, partial: live.generation?.message as AssistantMessage | undefined, spend: spendOf(usage), tool: slot && { name: slot.name, args: argsOf(items, slot.callId), details: slot.details }, activity };
}

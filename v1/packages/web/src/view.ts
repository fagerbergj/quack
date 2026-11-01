import type { AssistantMessage } from '@earendil-works/pi-ai';
import type { AgentEvent, MessageChange, SnapshotEvent } from '@earendil-works/pi-durable';

// The snapshot carries every entry, so a late viewer rebuilds the whole transcript from it.
export type View = { entries: SnapshotEvent['entries']; running: boolean; partial?: AssistantMessage; tokens?: number };

export const emptyView: View = { entries: [], running: false };

function change(msg: AssistantMessage, c: MessageChange): AssistantMessage {
  if (c.type === 'message') return c.message;
  const content = [...msg.content];
  if (c.type === 'text_delta' || c.type === 'thinking_delta') {
    const block = content[c.contentIndex];
    if (block?.type === 'text') content[c.contentIndex] = { ...block, text: block.text + c.delta };
    if (block?.type === 'thinking') content[c.contentIndex] = { ...block, thinking: block.thinking + c.delta };
  } else if ('block' in c) {
    content[c.contentIndex] = c.block;
  }
  return { ...msg, content };
}

const withEntry = (entries: View['entries'], entry: View['entries'][number]) =>
  entries.some((e) => e.id === entry.id) ? entries : [...entries, entry];

// Input plus output tokens across every model bucket of the snapshot's usage.
const tokensOf = (s: SnapshotEvent) => Object.values(s.usage?.models ?? {}).reduce((n, u) => n + (u.input ?? 0) + (u.output ?? 0), 0);

export const snapshotView = (s: SnapshotEvent): View => ({ entries: s.entries, running: s.run !== undefined, partial: s.generation?.message, tokens: tokensOf(s) });

function step(v: View, e: AgentEvent): View {
  switch (e.type) {
    case 'snapshot': return snapshotView(e);
    case 'run_start': return { ...v, running: true };
    case 'run_end': return { ...v, running: false, partial: undefined };
    case 'message_start': return e.message.role === 'assistant' ? { ...v, partial: e.message } : v;
    case 'message_update': return v.partial ? { ...v, partial: e.changes.reduce(change, v.partial) } : v;
    case 'message_end': return { ...v, entries: withEntry(v.entries, e.entry), partial: undefined };
    case 'entry_appended': return { ...v, entries: withEntry(v.entries, e.entry) };
    case 'tool_execution_end': return e.entry ? { ...v, entries: withEntry(v.entries, e.entry) } : v;
    default: return v;
  }
}

export const applyEvents = (v: View, events: readonly AgentEvent[]) => events.reduce(step, v);

// Tool activity is when an approval can appear or clear, so the inbox refetches on it.
export const touchesApprovals = (events: readonly AgentEvent[]) =>
  events.some((e) => e.type === 'tool_execution_start' || e.type === 'tool_execution_end' || e.type === 'run_end');

const POSTED = /^Posted review v(\d+) as \w+: (https:\/\/\S+)$/;

/** The URL post_review reports, from its result text. */
export const postedUrl = (text: string) => POSTED.exec(text.trim())?.[2];

/** Review version -> posted URL, read off the post_review results in the entries. */
export const postedVersions = (entries: View['entries']) =>
  new Map(entries.flatMap((e) => (e.model ?? []).flatMap((m) => {
    const hit = m.role === 'toolResult' && !m.isError ? POSTED.exec(m.content.map((c) => (c.type === 'text' ? c.text : '')).join('').trim()) : null;
    return hit ? [[Number(hit[1]), hit[2]!] as const] : [];
  })));

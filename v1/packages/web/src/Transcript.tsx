import { useState, type ReactNode } from 'react';
import type { AssistantMessage, Message, ToolResultMessage } from '@earendil-works/pi-ai';
import { Markdown } from './Markdown.tsx';
import { postedUrl, type View } from './view.ts';

// Session view: user bubble, plain assistant prose, ONE collapsed activity line per run of work (thinking, tools),
// thin muted status lines (compacted); cards only for attention. 20px between turns, 8px inside; 14.5/13/12 type.

type Call = Extract<AssistantMessage['content'][number], { type: 'toolCall' }>;
type Step = { t: 'think'; text: string } | { t: 'call'; call: Call; result?: ToolResultMessage } | { t: 'orphan'; result: ToolResultMessage };
type Item =
  | { k: 'user'; key: string; m: Message }
  | { k: 'compact'; key: string; m: Message }
  | { k: 'prose'; key: string; text: string }
  | { k: 'error'; key: string; text: string }
  | { k: 'work'; key: string; start: number; steps: Step[] };

const text = (content: string | readonly { type: string; text?: string }[]) =>
  typeof content === 'string' ? content : content.map((c) => (c.type === 'text' ? c.text : `[${c.type}]`)).join('');

const LIMIT = 2000;
const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)}...` : s);

// The first string or string-list argument is usually the query, URL or command.
function hint(args: Record<string, unknown>) {
  const v = Object.values(args).find((a) => typeof a === 'string' || (Array.isArray(a) && a.every((x) => typeof x === 'string')));
  return v === undefined ? '' : clip([v].flat().join(', '), 80);
}

const meta = 'text-[13px] text-muted';
const chars = (n: number) => (n >= 1000 ? `${Math.round(n / 1000)} k chars` : `${n} chars`);
const quiet = 'cursor-pointer text-[13px] text-muted hover:text-ink';
const sum = 'group flex min-h-11 cursor-pointer list-none items-center gap-1.5 lg:min-h-7 [&::-webkit-details-marker]:hidden';

// Output that opens like a markdown document (heading, list) renders as one; anything else stays monospace.
const MD_START = /^\s*(#{1,6} |[-*] |\d+\. )/;

function Chevron() {
  return <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="chev flex-none transition-transform"><path d="M9 6l6 6-6 6" /></svg>;
}

function Body({ body, markdown }: { body: string; markdown?: boolean }) {
  const [all, setAll] = useState(false);
  const [raw, setRaw] = useState(false);
  const long = body.length > LIMIT;
  const rendered = markdown && !raw;
  return (
    <>
      {markdown && <button type="button" onClick={() => setRaw(!raw)} className={`min-h-11 lg:min-h-0 ${quiet}`}>{raw ? 'Rendered' : 'Raw'}</button>}
      {rendered ? <div className="text-[13.5px]"><Markdown text={body} /></div> : (
        <pre className="m-0 max-w-full overflow-x-auto rounded-lg bg-soft p-2.5 font-mono text-[12px] leading-relaxed whitespace-pre-wrap text-ink-2">{all || !long ? body : clip(body, LIMIT)}</pre>
      )}
      {long && !all && !rendered && <button type="button" onClick={() => setAll(true)} className={`min-h-11 text-left lg:min-h-0 ${quiet}`}>{`Show all (${body.length} characters)`}</button>}
    </>
  );
}

function CallRow({ call, result }: { call: Call; result?: ToolResultMessage }) {
  const out = result && text(result.content);
  const url = result && !result.isError && out ? postedUrl(out) : undefined;
  const note = !result ? 'running' : result.isError ? 'error' : chars(out!.length);
  return (
    <details className="text-[13px]">
      <summary className={sum}>
        <Chevron />
        <span className="font-mono text-[12px] text-ink">{call.name}</span>
        <span className="min-w-0 flex-1 truncate text-ink-2">{hint(call.arguments)}</span>
        {url && <a href={url} target="_blank" rel="noopener noreferrer" className="whitespace-nowrap text-ink-2 hover:text-ink">Posted ↗</a>}
        <span className={`whitespace-nowrap ${result?.isError ? 'text-danger' : meta}`}>{note}</span>
      </summary>
      <div className="mb-2 ml-[18px] flex flex-col gap-2">
        <Body body={call.argumentsError ?? JSON.stringify(call.arguments, null, 2)} />
        {out !== undefined && <Body body={out} markdown={!result!.isError && MD_START.test(out)} />}
      </div>
    </details>
  );
}

const countBy = (names: string[]) => [...Map.groupBy(names, (n) => n)].map(([n, v]) => (v.length > 1 ? `${n} ×${v.length}` : n)).join(', ');

function duration(start: number, steps: Step[]) {
  const ends = steps.flatMap((s) => (s.t === 'call' && s.result ? [s.result.timestamp] : s.t === 'orphan' ? [s.result.timestamp] : []));
  const ms = ends.length ? Math.max(...ends) - start : 0;
  return ms >= 1000 ? ` ${Math.round(ms / 1000)} s` : '';
}

// Everything between two prose blocks, as one line that opens into a timeline.
const SHOWN = 5;

function Activity({ start, steps }: { start: number; steps: Step[] }) {
  const [all, setAll] = useState(false);
  const shown = all ? steps : steps.slice(0, SHOWN);
  const names = steps.flatMap((s) => (s.t === 'call' ? [s.call.name] : s.t === 'orphan' ? [s.result.toolName] : []));
  const parts = [names.length ? `Worked${duration(start, steps)}` : 'Thought', names.length ? `${names.length} ${names.length === 1 ? 'tool' : 'tools'}` : '', countBy(names)].filter(Boolean);
  return (
    <details>
      <summary className={`${sum} ${meta} hover:text-ink`}>
        <Chevron />
        <span className="min-w-0 truncate">{parts.join(' · ')}</span>
      </summary>
      <div className="mt-1.5 ml-1.5 flex flex-col gap-1 border-l-2 border-line pl-4">
        {shown.map((s, i) => {
          if (s.t === 'think') return <p key={i} className="m-0 text-[13px] whitespace-pre-wrap text-muted italic">{s.text}</p>;
          if (s.t === 'call') return <CallRow key={s.call.id} call={s.call} result={s.result} />;
          return <CallRow key={i} call={{ type: 'toolCall', id: s.result.toolCallId, name: s.result.toolName, arguments: {} }} result={s.result} />;
        })}
        {steps.length > shown.length && <button type="button" onClick={() => setAll(true)} className={`min-h-11 self-start text-left lg:min-h-0 ${quiet}`}>{`+ ${steps.length - shown.length} more`}</button>}
      </div>
    </details>
  );
}

// pi-durable writes a compaction as a user-role entry; it is context for the model, not something the user said.
function Compacted({ m }: { m: Message }) {
  const at = new Date(m.timestamp);
  return (
    <details className={meta}>
      <summary className={sum}>
        <Chevron />
        <span>Earlier conversation summarized</span>
        <time dateTime={at.toISOString()} className="ml-auto">{at.toLocaleString()}</time>
      </summary>
      <div className="ml-[18px] text-ink-2"><Markdown text={m.role === 'user' ? text(m.content) : ''} /></div>
    </details>
  );
}

const DUCK = (
  <svg width="26" height="18" viewBox="0 0 64 48" aria-hidden="true">
    <ellipse cx="34" cy="32" rx="22" ry="11" fill="#fdfcf7" stroke="#2f3a36" strokeOpacity=".25" />
    <circle cx="20" cy="18" r="10" fill="#1f7a63" />
    <rect x="11" y="25" width="18" height="3" rx="1.5" fill="#fff" />
    <path d="M12 15 L1 19 L12 22 Z" fill="#e3a92b" />
    <circle cx="17" cy="15" r="1.8" fill="#fff" />
    <path d="M50 28 Q60 24 58 34" fill="#fdfcf7" />
  </svg>
);
// The duck paddles while a run is active or waits on the user; reduced-motion stops it.
export function Working({ label }: { label: string }) {
  return (
    <p aria-label={label} className="ducklings m-0 flex items-center gap-2 text-[13px] text-muted">
      {DUCK}
      {label}
    </p>
  );
}

function Row({ item }: { item: Item }): ReactNode {
  switch (item.k) {
    case 'user':
      return (
        <article aria-label="You" className="max-w-[88%] self-end rounded-[20px] rounded-br-[6px] bg-ink px-3.5 py-2.5 text-[14.5px] leading-relaxed text-on-ink lg:max-w-[80%] lg:px-4 [&_.md_code]:bg-on-ink/10 [&_a]:text-on-ink [&_a]:underline">
          <Markdown text={item.m.role === 'user' ? text(item.m.content) : ''} />
        </article>
      );
    case 'compact': return <Compacted m={item.m} />;
    case 'prose': return <article aria-label="Assistant" className="min-w-0 text-[14.5px] leading-relaxed text-ink"><Markdown text={item.text} /></article>;
    case 'error': return <p className="m-0 text-[13px] text-danger">{`Error: ${item.text}`}</p>;
    case 'work': return <Activity start={item.start} steps={item.steps} />;
  }
}

function blocks(m: AssistantMessage, key: string, results: Map<string, ToolResultMessage>): (Item | Step)[] {
  const out: (Item | Step)[] = m.content.flatMap((b, i): (Item | Step)[] => {
    if (b.type === 'thinking') return b.thinking.trim() ? [{ t: 'think', text: b.thinking }] : [];
    if (b.type === 'text') return b.text.trim() ? [{ k: 'prose', key: `${key}:${i}`, text: b.text }] : [];
    return b.type === 'toolCall' ? [{ t: 'call', call: b, result: results.get(b.id) }] : [];
  });
  return m.errorMessage ? [{ k: 'error', key: `${key}:err`, text: m.errorMessage }, ...out] : out;
}

// Consecutive steps fold into one work item; prose, user lines and compactions end it.
function build(view: View): Item[] {
  const messages = view.entries.flatMap((e) => (e.model ?? []).map((m, i) => ({ key: `${e.id}:${i}`, m, compaction: e.kind === 'pi.compaction' })));
  if (view.partial) messages.push({ key: 'partial', m: view.partial, compaction: false });
  const results = new Map(messages.flatMap(({ m }) => (m.role === 'toolResult' ? [[m.toolCallId, m] as const] : [])));
  const called = new Set(messages.flatMap(({ m }) => (m.role === 'assistant' ? m.content.flatMap((b) => (b.type === 'toolCall' ? [b.id] : [])) : [])));
  const items: Item[] = [];
  const add = (key: string, start: number, x: Item | Step) => {
    if ('k' in x) return void items.push(x);
    const last = items.at(-1);
    if (last?.k === 'work') last.steps.push(x);
    else items.push({ k: 'work', key, start, steps: [x] });
  };
  for (const { key, m, compaction } of messages) {
    if (m.role === 'user') items.push({ k: compaction ? 'compact' : 'user', key, m });
    else if (m.role === 'assistant') blocks(m, key, results).forEach((x) => add(key, m.timestamp, x));
    else if (m.role === 'toolResult' && !called.has(m.toolCallId)) add(key, m.timestamp, { t: 'orphan', result: m });
  }
  return items;
}

export function Transcript({ view }: { view: View }) {
  return (
    <div role="log" aria-label="Transcript" className="flex min-w-0 flex-col gap-5">
      {build(view).map((item) => <Row key={item.key} item={item} />)}
    </div>
  );
}

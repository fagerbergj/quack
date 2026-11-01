import { useState } from 'react';
import type { AssistantMessage, Message, ToolResultMessage } from '@earendil-works/pi-ai';
import { Markdown } from './Markdown.tsx';
import type { View } from './view.ts';

type Call = Extract<AssistantMessage['content'][number], { type: 'toolCall' }>;

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

function Tool({ label, note, body }: { label: string; note: string; body: string }) {
  const [all, setAll] = useState(false);
  const long = body.length > LIMIT;
  return (
    <details className="rounded-lg border border-line bg-surface px-3 py-2 text-[13px]">
      <summary className="flex min-h-6 cursor-pointer list-none items-center gap-2 [&::-webkit-details-marker]:hidden">
        <span className="font-mono text-[12px] text-ink">{label}</span>
        <span className={`min-w-0 truncate ${meta}`}>{note}</span>
      </summary>
      <pre className="m-0 mt-2 max-w-full overflow-x-auto rounded-lg bg-soft p-2.5 font-mono text-[12px] leading-relaxed whitespace-pre-wrap text-ink-2">{all || !long ? body : clip(body, LIMIT)}</pre>
      {long && !all && <button type="button" onClick={() => setAll(true)} className="mt-2 min-h-11 cursor-pointer rounded-lg border border-line bg-surface px-3 text-xs font-medium text-ink-2 hover:text-ink lg:min-h-0 lg:py-1">{`Show all (${body.length} characters)`}</button>}
    </details>
  );
}

const countBy = (names: string[]) => [...Map.groupBy(names, (n) => n)].map(([n, v]) => `${n} ×${v.length}`).join(', ');

function duration(sentAt: number, results: ToolResultMessage[]) {
  const ms = Math.max(0, ...results.map((r) => r.timestamp)) - sentAt;
  return ms >= 1000 ? `${Math.round(ms / 1000)} s` : '';
}

// One card per assistant turn: a summary line, and the per-call rows once opened.
function ToolCard({ calls, results, sentAt }: { calls: Call[]; results: Map<string, ToolResultMessage>; sentAt: number }) {
  const mine = calls.flatMap((c) => results.get(c.id) ?? []);
  const names = calls.map((c) => c.name);
  const summary = calls.length === 1 ? hint(calls[0]!.arguments) : `${calls.length} calls · ${countBy(names)}`;
  return (
    <details className="rounded-xl border border-line bg-surface px-3.5 py-2.5 text-[13px]">
      <summary className="flex min-h-6 cursor-pointer list-none items-center gap-2 [&::-webkit-details-marker]:hidden">
        {calls.length === 1 && <span className="font-mono text-[12px] text-ink">{names[0]}</span>}
        <span className={`min-w-0 truncate ${meta}`}>{summary}</span>
        <span className={`ml-auto whitespace-nowrap tabular-nums ${meta}`}>{duration(sentAt, mine)}</span>
      </summary>
      <div className="mt-2 flex flex-col gap-2">
        {calls.map((c) => {
          const r = results.get(c.id);
          const body = r && text(r.content);
          return (
            <div key={c.id} className="flex flex-col gap-2">
              <Tool label={`Tool call: ${c.name}${c.argumentsError ? ' (bad arguments)' : ''}`} note={hint(c.arguments)} body={c.argumentsError ?? JSON.stringify(c.arguments, null, 2)} />
              {r && body !== undefined && <ResultRow r={r} body={body} />}
            </div>
          );
        })}
      </div>
    </details>
  );
}

function ResultRow({ r, body }: { r: ToolResultMessage; body: string }) {
  const label = `Tool result: ${r.toolName}${r.isError ? ' (error)' : ''}`;
  return <Tool label={label} note={r.isError ? clip(body.split('\n')[0] ?? '', 80) : `${body.length} characters`} body={body} />;
}

function Thinking({ text: t }: { text: string }) {
  return (
    <details className="text-[12px] text-muted">
      <summary className="inline-flex min-h-6 cursor-pointer list-none items-center rounded-md bg-soft px-2 [&::-webkit-details-marker]:hidden">Thinking</summary>
      <p className="mt-1 mb-0 whitespace-pre-wrap">{t}</p>
    </details>
  );
}

function Assistant({ m, results }: { m: AssistantMessage; results: Map<string, ToolResultMessage> }) {
  const thinking = m.content.flatMap((b) => (b.type === 'thinking' && b.thinking.trim() ? [b.thinking] : [])).join('\n\n');
  const calls = m.content.filter((b): b is Call => b.type === 'toolCall');
  return (
    <>
      {m.errorMessage && <p className="m-0 rounded-xl border border-danger-line bg-danger-bg/60 px-3.5 py-2.5 text-[13px] text-danger">{`Error: ${m.errorMessage}`}</p>}
      {thinking && <Thinking text={thinking} />}
      {m.content.map((b, i) => (b.type === 'text' && b.text.trim() ? <Markdown key={i} text={b.text} /> : null))}
      {calls.length > 0 && <ToolCard calls={calls} results={results} sentAt={m.timestamp} />}
    </>
  );
}

const DUCK = (
  <svg width="30" height="22" viewBox="0 0 64 48" aria-hidden="true">
    <ellipse cx="34" cy="32" rx="22" ry="11" fill="#fdfcf7" stroke="#2f3a36" strokeOpacity=".25" />
    <circle cx="20" cy="18" r="10" fill="#1f7a63" />
    <rect x="11" y="25" width="18" height="3" rx="1.5" fill="#fff" />
    <path d="M12 15 L1 19 L12 22 Z" fill="#e3a92b" />
    <circle cx="17" cy="15" r="1.8" fill="#fff" />
    <path d="M50 28 Q60 24 58 34" fill="#fdfcf7" />
  </svg>
);
const DUCKLING = (
  <svg width="18" height="14" viewBox="0 0 18 14" aria-hidden="true">
    <ellipse cx="10" cy="9" rx="7" ry="4.5" fill="#e9c46a" />
    <circle cx="5.5" cy="5" r="3.5" fill="#e9c46a" />
    <path d="M2.4 4.3 L0 5.2 L2.4 6.2 Z" fill="#d08a1d" />
    <circle cx="4.4" cy="4.2" r="0.8" fill="#0f1513" />
  </svg>
);

// Shown while a run is active; the animation stops under prefers-reduced-motion.
function Working() {
  return (
    <div className="ducklings flex items-end gap-1.5 text-[12px] text-muted" aria-label="Working">
      {DUCK}{DUCKLING}{DUCKLING}{DUCKLING}
      <span className="ml-1">working…</span>
    </div>
  );
}

function Line({ m, results, orphan }: { m: Message; results: Map<string, ToolResultMessage>; orphan: boolean }) {
  if (m.role === 'user') {
    return (
      <article aria-label="You" className="max-w-[85%] self-end rounded-[22px] rounded-br-md bg-ink px-3.5 py-2.5 text-[14.5px] leading-relaxed whitespace-pre-wrap text-on-ink">
        {text(m.content)}
      </article>
    );
  }
  if (m.role === 'assistant') {
    return <article aria-label="Assistant" className="flex max-w-[95%] min-w-0 flex-col gap-3 text-[14.5px] leading-relaxed text-ink"><Assistant m={m} results={results} /></article>;
  }
  // A result whose call is not in the view (e.g. a truncated log) still shows on its own.
  return m.role === 'toolResult' && orphan ? <ResultRow r={m} body={text(m.content)} /> : null;
}

export function Transcript({ view }: { view: View }) {
  const messages = view.entries.flatMap((e) => (e.model ?? []).map((m, i) => ({ key: `${e.id}:${i}`, m })));
  const results = new Map(messages.flatMap(({ m }) => (m.role === 'toolResult' ? [[m.toolCallId, m] as const] : [])));
  const called = new Set(messages.flatMap(({ m }) => (m.role === 'assistant' ? m.content.flatMap((b) => (b.type === 'toolCall' ? [b.id] : [])) : [])));
  return (
    <div role="log" aria-label="Transcript" className="flex min-w-0 flex-col gap-4">
      {messages.map(({ key, m }) => <Line key={key} m={m} results={results} orphan={m.role === 'toolResult' && !called.has(m.toolCallId)} />)}
      {view.partial && <Line m={view.partial} results={results} orphan={false} />}
      {view.running && <Working />}
    </div>
  );
}

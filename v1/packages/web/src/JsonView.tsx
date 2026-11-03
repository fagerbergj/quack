import type { ReactNode } from 'react';
import { Markdown } from './Markdown.tsx';

type Json = string | number | boolean | null | Json[] | { [k: string]: Json };

const URL_ONLY = /^https?:\/\/\S+$/;
const OPEN_DEPTH = 2;

/** The parsed value when text is a JSON object or array; scalars stay prose. */
export function parseJson(text: string): (Json & object) | undefined {
  try {
    const v = JSON.parse(text) as Json;
    return typeof v === 'object' && v !== null ? v : undefined;
  } catch {
    return undefined;
  }
}

const entriesOf = (v: Json & object): [string, Json][] => (Array.isArray(v) ? v.map((x, i): [string, Json] => [String(i), x]) : Object.entries(v));
const isObj = (v: Json | undefined): v is { [k: string]: Json } => typeof v === 'object' && v !== null && !Array.isArray(v);

// A person reads a list item by its name, not its index.
function label(v: Json): string {
  if (!isObj(v)) return '';
  const n = v.name ?? v.title ?? (isObj(v.player) ? v.player.name : undefined);
  return typeof n === 'string' ? n : '';
}

function Scalar({ v }: { v: string | number | boolean | null }) {
  if (v === null) return <span className="italic text-muted">null</span>;
  if (typeof v === 'number') return <span className="tabular-nums text-info">{v}</span>;
  if (typeof v === 'boolean') return <span className="text-warning">{String(v)}</span>;
  if (URL_ONLY.test(v)) return <a href={v} target="_blank" rel="noreferrer" className="text-link [overflow-wrap:anywhere]">{v}</a>;
  return <span className="whitespace-pre-wrap text-ink [overflow-wrap:anywhere]">{v}</span>;
}

const Key = ({ k }: { k: string }) => <span className="font-mono text-[12px] text-accent-ink [overflow-wrap:anywhere]">{k}</span>;

function Node({ k, v, depth }: { k: string; v: Json; depth: number }) {
  if (typeof v !== 'object' || v === null) {
    return <div className="flex min-w-0 flex-col gap-0.5 py-1 sm:flex-row sm:gap-2"><span className="flex-none sm:max-w-[40%]"><Key k={k} /></span><span className="min-w-0 text-[13px]"><Scalar v={v} /></span></div>;
  }
  const items = entriesOf(v);
  const unit = Array.isArray(v) ? 'item' : 'key';
  const hint = label(v);
  return (
    <details open={depth < OPEN_DEPTH} className="min-w-0">
      <summary className="flex min-h-11 cursor-pointer list-none items-center gap-2 lg:min-h-7 [&::-webkit-details-marker]:hidden">
        <span aria-hidden="true" className="text-[10px] text-muted">▸</span>
        <Key k={k} />
        {/* The open node shows the named field itself, so the hint is for the folded one. */}
        {hint && <span className="min-w-0 truncate text-[13px] text-ink [details[open]>summary>&]:hidden">{hint}</span>}
        <span className="ml-auto flex-none text-[11px] text-muted">{`${items.length} ${unit}${items.length === 1 ? '' : 's'}`}</span>
      </summary>
      <div className="ml-1.5 border-l border-line pl-3">
        {items.map(([ck, cv]) => <Node key={ck} k={ck} v={cv} depth={depth + 1} />)}
      </div>
    </details>
  );
}

/** A collapsible tree of any JSON object or array; top levels open, deeper ones fold. */
export function JsonView({ value }: { value: Json & object }) {
  return <div role="group" aria-label="JSON" className="flex min-w-0 flex-col">{entriesOf(value).map(([k, v]) => <Node key={k} k={k} v={v} depth={0} />)}</div>;
}

/** Saved text as the reader should see it: JSON as a tree, anything else as markdown. */
export function Content({ text }: { text: string }): ReactNode {
  const json = parseJson(text);
  return json === undefined ? <Markdown text={text} /> : <JsonView value={json} />;
}

/** Tool-call or approval arguments in a soft box; a non-object value shows as a single scalar. */
export function Args({ value }: { value: unknown }) {
  const v = value as Json | undefined;
  if (typeof v === 'object' && v !== null && !Object.keys(v).length) return null;
  return <div className="max-h-60 min-w-0 overflow-y-auto rounded-lg bg-soft p-2.5">{typeof v === 'object' && v !== null ? <JsonView value={v} /> : <Scalar v={v === undefined ? null : v} />}</div>;
}

import { useState, type ReactNode } from 'react';
import { chev } from '../../CheckResult.tsx';
import { wordDiff, type Line } from '../../lineDiff.ts';
import { useMark } from './diff.ts';
import { Info, Inline, Reasons } from './Info.tsx';
import { MARKED, MarkTag } from './SchemaTree.tsx';
import type { ContractSpec } from './spec.ts';

export const label = 'text-xs font-semibold uppercase tracking-wider text-muted';
export const card = 'rounded-[18px] border border-line bg-surface';
const summary = 'flex min-h-11 cursor-pointer list-none items-center gap-2 [&::-webkit-details-marker]:hidden focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-link';
const row = 'border-t border-line first:border-t-0';

/** A section closed to one line, its title and count, until opened. */
export function Fold({ title, count, open, onToggle, children }: { title: string; count?: number; open?: boolean; onToggle?: (open: boolean) => void; children: ReactNode }) {
  return (
    <details open={open} onToggle={onToggle && ((e) => onToggle(e.currentTarget.open))} className={card}>
      <summary className={`${summary} rounded-[18px] px-4`}>
        {chev}<span className={label}>{title}</span>
        {count !== undefined && <span className="rounded-full bg-soft px-2 text-[11.5px] font-semibold text-ink-2 tabular-nums">{count}</span>}
      </summary>
      <div className="border-t border-line">{children}</div>
    </details>
  );
}

/** One row that shows its headline clamped to two lines, and opens for the rest. */
function Row({ head, children }: { head: ReactNode; children: ReactNode }) {
  return (
    <li className={row}>
      <details className="group">
        <summary className={`${summary} items-start px-4 py-2.5`}>
          <span className="mt-1">{chev}</span>
          <span className="flex min-w-0 flex-1 flex-col gap-0.5 text-sm leading-snug [overflow-wrap:anywhere] [&>.clamp]:line-clamp-2 group-open:[&>.clamp]:line-clamp-none">{head}</span>
        </summary>
        <div className="flex flex-col gap-2 px-4 pb-3 pl-10 text-[13px] leading-snug text-ink-2 [overflow-wrap:anywhere]">{children}</div>
      </details>
    </li>
  );
}

function Gap({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  if (open) return text;
  const n = text.match(/\S+/g)?.length ?? 0;
  return <><button type="button" aria-label={`Show ${n} unchanged words`} title="Show the unchanged words" onClick={() => setOpen(true)} className="cursor-pointer rounded border-0 bg-soft px-1.5 font-[inherit] text-muted hover:text-ink">…</button>{/\s$/.test(text) ? ' ' : ''}</>;
}

// One side of a before/after pair; with a word diff, the words that side alone has are tinted.
function Side({ title, parts, keep }: { title: string; parts: Line[]; keep: 'del' | 'add' }) {
  return (
    <div className="flex min-w-0 flex-col gap-1 rounded-xl border border-line px-3 py-2">
      <span className={label}>{title}</span>
      <p className="m-0 text-[13.5px] leading-relaxed whitespace-pre-wrap text-ink [overflow-wrap:anywhere]">
        {parts.map((p, i) => {
          if (p.kind === 'gap') return <Gap key={i} text={p.text} />;
          if (p.kind === 'same') return <Inline key={i} text={p.text} />;
          if (p.kind !== keep) return null;
          return keep === 'del'
            ? <del key={i} className="rounded-sm bg-danger-bg decoration-danger/70"><span className="sr-only">Removed: </span>{p.text}</del>
            : <ins key={i} className="rounded-sm bg-success-bg no-underline"><span className="sr-only">Added: </span>{p.text}</ins>;
        })}
      </p>
    </div>
  );
}

// A short value (a type, a flag) reads best on one line, old struck beside new.
function OneLine({ was, now }: { was: string; now: string }) {
  return (
    <p className="m-0 font-mono text-[12.5px] [overflow-wrap:anywhere]">
      {was && <del className="rounded-sm bg-danger-bg px-0.5 decoration-danger/70"><span className="sr-only">Removed: </span>{was}</del>}
      {was && now && <span aria-hidden="true" className="mx-1.5 text-muted">→</span>}
      {now && <ins className="rounded-sm bg-success-bg px-0.5 no-underline"><span className="sr-only">Added: </span>{now}</ins>}
    </p>
  );
}

/** A text before and after, as two blocks; `words` tints what each side alone has, folds long unchanged runs, and puts a short value on one line. */
export function BeforeAfter({ was, now, words }: { was: string; now: string; words?: boolean }) {
  if (words && was.length < 60 && now.length < 60) return <OneLine was={was} now={now} />;
  const parts = words && was && now ? wordDiff(was, now) : undefined;
  return (
    <div className="grid gap-2 @[640px]:grid-cols-2">
      {was && <Side title="Before" parts={parts ?? [{ kind: 'same', text: was }]} keep="del" />}
      {now && <Side title={was ? 'After' : 'Added'} parts={parts ?? [{ kind: 'same', text: now }]} keep="add" />}
    </div>
  );
}

export const since = (spec: ContractSpec) => (spec.baseline === 'shipped' ? 'the shipped code' : `version ${spec.baseline ?? spec.version - 1}`);

/** The spec's own account of what changed: each one line, opening on before and after, why, and the decision behind it. */
export function ChangeRows({ spec }: { spec: ContractSpec }) {
  const byId = new Map(spec.choices.map((c) => [c.id, c]));
  return (
    <ul className="m-0 list-none p-0">
      {(spec.changes ?? []).map((c, i) => {
        const choice = byId.get(c.choice);
        return (
          <Row key={i} head={<>
            <code className="font-mono text-[11.5px] text-muted"><span className={c.was ? 'text-warning' : 'text-success'}>{c.was ? 'Changed' : 'Added'}</span> · {c.choice}</code>
            <span className="clamp"><Inline text={c.now} /></span>
          </>}>
            <BeforeAfter was={c.was} now={c.now} />
            <p className="m-0"><Inline text={c.why} /></p>
            {choice && <p className="m-0 flex items-center gap-1 text-muted">The decision behind it<Info choices={[choice]} /></p>}
          </Row>
        );
      })}
    </ul>
  );
}

export type Listed = { what: string; callers: string[]; migration?: string };
// Specs sealed before contract.spec v2 may say `change` for what and name their callers in one string.
export const listedBreaks = (spec: ContractSpec): Listed[] =>
  (spec.compatibility?.breaking ?? []).map((b) => ({ what: b.what ?? (b as { change?: string }).change ?? '', callers: [b.callers ?? []].flat(), migration: b.migration }));

/** The breaks the spec lists, each one line with its caller count, opening on the callers and the migration. */
export function BreakRows({ spec }: { spec: ContractSpec }) {
  return (
    <ul className="m-0 list-none p-0">
      {listedBreaks(spec).map((b, i) => (
        <Row key={i} head={<>
          <span className="clamp"><span aria-hidden="true" className="mr-1.5 text-warning">⚠</span><Inline text={b.what} /></span>
          <span className="text-[12px] text-muted">{b.callers.length} {b.callers.length === 1 ? 'caller' : 'callers'}{b.migration ? ' · migration' : ''}</span>
        </>}>
          <ul className="m-0 flex list-none flex-wrap gap-1.5 p-0">{b.callers.map((n) => <li key={n} className="rounded-full bg-soft px-2.5 py-0.5 text-xs [overflow-wrap:anywhere]">{n}</li>)}</ul>
          {b.migration && <p className="m-0"><span className="font-semibold text-ink">Migration:</span> <Inline text={b.migration} /></p>}
        </Row>
      ))}
    </ul>
  );
}

const List = ({ items }: { items: string[] }) => <ul className="m-0 flex list-disc flex-col gap-1 pl-5">{items.map((s) => <li key={s}><Inline text={s} /></li>)}</ul>;

function Intent({ spec }: { spec: ContractSpec }) {
  const i = spec.intent;
  const part = (title: string, body: ReactNode) => <section className="flex flex-col gap-1"><h4 className={`m-0 font-sans ${label}`}>{title}</h4>{body}</section>;
  return (
    <div className="flex flex-col gap-3 px-4 py-3 text-[13.5px] leading-relaxed [overflow-wrap:anywhere]">
      {part('Purpose', <p className="m-0"><Inline text={i.purpose} /></p>)}
      {i.audience && part('Audience', <p className="m-0"><Inline text={i.audience} /></p>)}
      {!!i.consumers?.length && part('Used by', <List items={i.consumers} />)}
      {!!i.guarantees?.length && part('Guarantees', <List items={i.guarantees} />)}
      {!!i.nonGoals?.length && part('Not in scope', <List items={i.nonGoals} />)}
    </div>
  );
}

/** What the contract is for, clamped to three lines; the Intent section holds the rest. */
export function Purpose({ spec }: { spec: ContractSpec }) {
  const m = useMark('#purpose');
  return (
    <section aria-label="What it does" className={`flex flex-col items-start gap-1 ${m ? `rounded-lg px-2 py-1 ${MARKED[m]}` : ''}`}>
      <MarkTag m={m} />
      <p className="m-0 line-clamp-3 text-[14.5px] leading-relaxed text-ink-2 [overflow-wrap:anywhere]"><Inline text={spec.intent.purpose} /></p>
    </section>
  );
}

/** Everything about a contract beyond its document, one counted line each. `told` leaves out the breaks and changes an approval card already shows. */
export function Summary({ spec, told }: { spec: ContractSpec; told?: boolean }) {
  const breaks = listedBreaks(spec).length, changes = spec.changes?.length ?? 0, safe = spec.compatibility?.safe ?? [];
  return (
    <div className="flex flex-col gap-2">
      {!told && breaks > 0 && <Fold title="Breaking changes" count={breaks}><BreakRows spec={spec} /></Fold>}
      {!told && changes > 0 && <Fold title={`Changed from ${since(spec)}`} count={changes}><ChangeRows spec={spec} /></Fold>}
      {safe.length > 0 && <Fold title="Unchanged, checked" count={safe.length}>
        <ul className="m-0 flex list-none flex-col gap-1.5 px-4 py-3 text-sm [overflow-wrap:anywhere]">{safe.map((s) => <li key={s}><span aria-hidden="true" className="mr-2 text-success">✓</span><Inline text={s} /></li>)}</ul>
      </Fold>}
      <Fold title="Intent"><Intent spec={spec} /></Fold>
      {spec.choices.length > 0 && <Fold title="Decisions" count={spec.choices.length}>
        <ul className="m-0 list-none p-0">{spec.choices.map((c) => <Row key={c.id} head={<>
          <code className="font-mono text-[11.5px] text-muted">{c.id}{c.pointer && ` · ${c.pointer}`}</code><span className="clamp"><Inline text={c.decision} /></span>
        </>}><Reasons c={c} bare /></Row>)}</ul>
      </Fold>}
    </div>
  );
}

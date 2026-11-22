import { useContext, type ReactNode } from 'react';
import { DiffContext, useMark, type Mark } from './diff.ts';
import { CommentButton, docTarget, type Target } from './Comments.tsx';
import { Info } from './Info.tsx';
import { Desc, Glyph, ShortChange, useFlash, useItem } from './Review.tsx';
import type { Choice, Field } from './spec.ts';

const fact = 'inline-flex items-start gap-1 rounded bg-soft px-1.5 py-0.5 text-xs text-muted [overflow-wrap:anywhere]';

// A code type is source text: it takes the width it needs, and `code` links the declarations it names.
type Code = (type: string) => ReactNode;

const TAG: Record<Mark, [string, string]> = { added: ['New', 'bg-success-bg text-success'], changed: ['Changed', 'bg-warning-bg text-warning'], breaking: ['Breaks a caller', 'bg-danger-bg text-danger'] };
export const MARKED: Record<Mark, string> = { added: 'bg-warm', changed: 'bg-warm', breaking: 'bg-danger-bg/60' };

/** A chip naming how an element differs from the version it is compared with. */
export function MarkTag({ m }: { m: Mark | undefined }) {
  return m ? <span className={`flex flex-none items-center gap-1 self-center rounded px-1.5 text-[10.5px] font-semibold tracking-wide uppercase ${TAG[m][1]}`}>{m === 'breaking' && <Glyph m={m} />}{TAG[m][0]}</span> : null;
}

// Each level steps in 20px with a guide line per ancestor, so a parent's children read as one block even at phone width.
const STEP = 20;
// Change dots share one gutter left of every guide, so they line up whatever the depth.
const GUTTER = 14;
const DOT: Record<Mark, string> = { added: 'h-1.5 w-1.5 rounded-full bg-warning', changed: 'h-1.5 w-1.5 rounded-full bg-warning', breaking: 'h-1.5 w-1.5 rotate-45 bg-danger' };

// One mark per change; the word is its tooltip and name. It sits in the name's own box so it is level with the name's first line,
// and its offset undoes the row's indent so every dot lands in the same gutter at the table's left edge.
function Dot({ m, depth }: { m: Mark; depth: number }) {
  return <span data-gutter style={{ left: `${-(GUTTER + depth * STEP + 14)}px` }} className="absolute top-0 flex h-[1lh] w-3.5 items-center justify-center"><span role="img" aria-label={TAG[m][0]} title={TAG[m][0]} className={DOT[m]} /></span>;
}

function Row({ f, choices, code, target, covered }: { f: Field; choices: Choice[]; code?: Code; target?: Target; covered: boolean }) {
  const m = useMark(f.ptr);
  const item = useItem(f.ptr);
  const flash = useFlash<HTMLLIElement>(f.ptr);
  return (
    <li ref={flash.ref} tabIndex={-1} data-ptr={f.ptr} aria-level={f.depth + 1} style={{ paddingLeft: `${GUTTER + f.depth * STEP + 14}px` }} className={`group/c relative flex scroll-mt-16 flex-wrap items-baseline gap-x-2 gap-y-1 border-t py-2 pr-1 text-[13.5px] outline-none first:border-t-0 ${f.depth ? 'border-line/50' : 'border-line'} ${m ? MARKED[m] : ''} ${flash.cls}`}>
      {Array.from({ length: f.depth }, (_, i) => <span key={i} data-guide aria-hidden="true" className="absolute top-0 bottom-0 w-px bg-line-strong" style={{ left: `${GUTTER + i * STEP + 10}px` }} />)}
      <code className={`relative font-mono [overflow-wrap:break-word] @xl:flex-none ${code ? '@xl:w-40' : '@xl:w-44'}`}>{f.name.split(/(?<=[a-z0-9])(?=[A-Z])/).flatMap((p, i) => (i ? [<wbr key={i} />, p] : [p]))}{m && !covered && <Dot m={m} depth={f.depth} />}</code>
      <span className={`font-mono text-[12.5px] text-success ${code ? 'min-w-0 [overflow-wrap:anywhere]' : '@xl:w-24 @xl:flex-none'}`}>{code ? code(f.type) : f.type}</span>
      {f.required && <span className="text-[11px] tracking-wide text-danger">REQUIRED</span>}
      {f.where && <span className={fact}>{f.where}</span>}
      {(f.desc || f.facts.length > 0) && <span className="flex min-w-0 flex-[1_1_8rem] flex-col gap-1 text-[13px] text-muted">
        {f.desc && <Desc text={f.desc} />}
        {f.facts.length > 0 && <span className="flex flex-wrap gap-1.5">{f.facts.map(([k, v]) => <span key={k} className={fact}><span className="flex-none whitespace-nowrap">{k}</span><code className="font-mono text-ink-2">{v}</code></span>)}</span>}
      </span>}
      {target && <CommentButton target={target} />}
      <Info choices={choices} />
      {item && <ShortChange item={item} />}
    </li>
  );
}

/** Shared by every contract kind: one row per field with its type, required, default, example and an info button where choices govern it;
 * `target` names what a comment on a row is about, and a row it gives none takes no comment. */
export function SchemaTree({ rows, whyAt, code, target = fieldTarget }: { rows: Field[]; whyAt: (ptr: string) => Choice[]; code?: Code; target?: (f: Field) => Target | undefined }) {
  const marks = useContext(DiffContext);
  // What an added element holds is new with it: only the topmost added pointer gets a dot.
  const added = [...(marks ?? [])].filter(([, m]) => m === 'added').map(([p]) => `${p}/`);
  const covered = rows.map((f) => added.some((p) => f.ptr.startsWith(p)));
  return <ul role="list" className="m-0 list-none p-0">{rows.map((f, i) => <Row key={f.ptr} f={f} choices={whyAt(f.ptr)} code={code} target={target(f)} covered={covered[i]} />)}</ul>;
}

// A field with no description is named by its type, so the comment sheet says what it is about.
export const fieldTarget = (f: Field) => docTarget(f.ptr, f.desc ?? `${f.name}: ${f.type}`);

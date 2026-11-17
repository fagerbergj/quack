import type { ReactNode } from 'react';
import { ClassDiagram } from './ClassDiagram.tsx';
import { DRAWN } from './class-model.ts';
import { CommentButton, docTarget, type Target } from './Comments.tsx';
import { choicesAt, Info } from './Info.tsx';
import { Desc, Glyph, ShortChange, useFlash, useItem, useReview } from './Review.tsx';
import { itemsUnder, worst } from './review.ts';
import { fieldTarget, MarkTag, SchemaTree } from './SchemaTree.tsx';
import type { Choice, Decl, Field, Model } from './spec.ts';

const GROUPS: Array<[string, Array<Decl['kind']>]> = [['Types', ['type', 'interface', 'enum', 'other']], ['Functions', ['function']], ['Constants', ['const']]];
const link = 'cursor-pointer border-0 bg-transparent p-0 font-mono text-[length:inherit] text-link underline decoration-link/40 underline-offset-2 hover:decoration-link';
const pill = 'rounded bg-soft px-1.5 py-0.5 text-[11px] tracking-wide text-muted uppercase';

function Linked({ text, names }: { text: string; names: Set<string> }) {
  const { show } = useReview();
  return <>{text.split(/([A-Za-z_$][\w$]*)/).map((t, i) => (names.has(t) ? <button key={i} type="button" onClick={() => show(`/${t}`)} className={link}>{t}</button> : t))}</>;
}

function Refs({ label, list, names }: { label: string; list: string[]; names: Set<string> }) {
  if (!list.length) return null;
  return <p className="m-0 text-xs text-muted">{label} {list.map((n, i) => <span key={n}>{i > 0 && ', '}<Linked text={n} names={names} /></span>)}</p>;
}

// The places the contracts app resolves a typescript pointer: an exported declaration, and the members, parameters and enum members under
// it, never a union's variants or a function's return; a const's own fields are not members.
const MEMBERED = new Set<Decl['kind']>(['interface', 'type', 'function', 'enum']);
export const declTarget = (d: Decl): Target | undefined => (d.exported ? docTarget(`/${d.name}`, d.doc) : undefined);
export function rowTarget(d: Decl, f: Field): Target | undefined {
  const rest = f.ptr.slice(d.name.length + 2);
  if (!d.exported || !MEMBERED.has(d.kind) || /(^|\/)\d+(\/|$)/.test(rest) || (d.kind === 'function' && /^returns(\/|$)/.test(rest))) return undefined;
  return fieldTarget(f);
}

// A declaration's card carries the strongest mark of anything changed in it; its own change, before and after, sits under its description.
function Card({ d, names, usedBy, choices }: { d: Decl; names: Set<string>; usedBy: string[]; choices: Choice[] }) {
  const ptr = `/${d.name}`;
  const { items, centre } = useReview();
  const m = worst(itemsUnder(items, ptr));
  const own = useItem(ptr);
  const flash = useFlash<HTMLElement>(ptr);
  const code = (t: string): ReactNode => <Linked text={t} names={names} />;
  return (
    <article ref={flash.ref} id={`decl-${d.name}`} data-ptr={ptr} tabIndex={-1} aria-label={d.name} className={`-mx-2 flex scroll-mt-16 flex-col gap-2 rounded-lg border-t border-line px-2 pt-3 outline-none first:border-t-0 first:pt-0 ${flash.cls}`}>
      <div className="group/c flex flex-wrap items-center gap-2">
        <Glyph m={m} />
        <h3 className="m-0 font-mono text-[15px] font-semibold [overflow-wrap:anywhere]">{d.name}<span className="font-normal text-muted">{d.generics}</span></h3>
        {d.kind !== 'other' && <span className={pill}>{d.kind}</span>}
        {!d.exported && <span className={pill}>not exported</span>}
        <MarkTag m={own?.breaking ? 'breaking' : own?.mark === 'removed' ? undefined : own?.mark} />
        <span className="flex-1" />
        {declTarget(d) && <CommentButton target={declTarget(d)!} />}
        <Info choices={choices} />
      </div>
      {d.doc && <p className="m-0 text-[14px] leading-relaxed text-ink-2"><Desc text={d.doc} name="Full description" /></p>}
      {own && <ShortChange item={own} />}
      {d.text && <code className="font-mono text-[12.5px] text-success [overflow-wrap:anywhere]">{d.rows.length ? 'includes ' : '= '}{code(d.text)}</code>}
      {d.rows.length > 0 && <SchemaTree rows={d.rows} whyAt={() => []} code={code} target={(f) => rowTarget(d, f)} />}
      <Refs label="Uses" list={d.uses} names={names} />
      <Refs label="Used by" list={usedBy} names={names} />
      {/* Some types sit apart from the rest, where no tap in the diagram reaches them; this does. */}
      {DRAWN.has(d.kind) && <button type="button" onClick={() => centre?.(ptr)} className="-my-2 min-h-11 cursor-pointer self-start border-0 bg-transparent p-0 text-left text-[13px] text-link underline underline-offset-2 focus-ring">Show in diagram</button>}
    </article>
  );
}

/** A code contract of any language, once parsed into declarations: its types as a class diagram, then the changes, then every declaration as a card. */
export function CodeContract({ model, choices, changes }: { model: Model; choices: Choice[]; changes: ReactNode }) {
  const names = new Set(model.decls.map((d) => d.name));
  const usedBy = (n: string) => model.decls.filter((d) => d.uses.includes(n)).map((d) => d.name);
  return (
    <div className="flex min-w-0 flex-col gap-6">
      <ClassDiagram decls={model.decls} />
      {changes}
      <section aria-label="Declarations" className="flex min-w-0 flex-col gap-5 rounded-[18px] border border-line bg-surface p-4">
        {model.imports.length > 0 && <p className="m-0 text-xs text-muted">Names from elsewhere: {model.imports.map((i) => `${i.names.join(', ')} (${i.from})`).join('; ')}</p>}
        {GROUPS.map(([title, kinds]) => {
          const ds = model.decls.filter((d) => kinds.includes(d.kind));
          return ds.length > 0 && (
            <div key={title} className="flex flex-col gap-3">
              <h2 className="m-0 text-[17px] font-semibold">{title} <span className="font-normal text-muted">{ds.length}</span></h2>
              {ds.map((d, i) => <Card key={`${d.name}-${i}`} d={d} names={names} usedBy={usedBy(d.name)} choices={choicesAt(choices, `/${d.name}`)} />)}
            </div>
          );
        })}
      </section>
    </div>
  );
}

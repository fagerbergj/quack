import type { ReactNode } from 'react';
import { ClassDiagram } from './ClassDiagram.tsx';
import { choicesAt, Info, Inline } from './Info.tsx';
import { SchemaTree } from './SchemaTree.tsx';
import type { Choice, Decl, Model } from './spec.ts';

const GROUPS: Array<[string, Array<Decl['kind']>]> = [['Types', ['type', 'interface', 'enum', 'other']], ['Functions', ['function']], ['Constants', ['const']]];
const link = 'cursor-pointer border-0 bg-transparent p-0 font-mono text-[length:inherit] text-link underline decoration-link/40 underline-offset-2 hover:decoration-link';
const pill = 'rounded bg-soft px-1.5 py-0.5 text-[11px] tracking-wide text-muted uppercase';

// The app routes on the hash, so a jump scrolls and focuses rather than following a #fragment.
export function jumpTo(name: string) {
  const el = document.getElementById(`decl-${name}`);
  el?.scrollIntoView?.({ behavior: 'smooth', block: 'start' });
  el?.focus({ preventScroll: true });
}

function Linked({ text, names }: { text: string; names: Set<string> }) {
  return <>{text.split(/([A-Za-z_$][\w$]*)/).map((t, i) => (names.has(t) ? <button key={i} type="button" onClick={() => jumpTo(t)} className={link}>{t}</button> : t))}</>;
}


function Refs({ label, list, names }: { label: string; list: string[]; names: Set<string> }) {
  if (!list.length) return null;
  return <p className="m-0 text-xs text-muted">{label} {list.map((n, i) => <span key={n}>{i > 0 && ', '}<Linked text={n} names={names} /></span>)}</p>;
}

function Card({ d, names, usedBy, choices }: { d: Decl; names: Set<string>; usedBy: string[]; choices: Choice[] }) {
  const code = (t: string): ReactNode => <Linked text={t} names={names} />;
  return (
    <article id={`decl-${d.name}`} tabIndex={-1} aria-label={d.name} className="flex scroll-mt-4 flex-col gap-2 border-t border-line pt-3 outline-none first:border-t-0 first:pt-0 focus-visible:shadow-ring">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="m-0 font-mono text-[15px] font-semibold [overflow-wrap:anywhere]">{d.name}<span className="font-normal text-muted">{d.generics}</span></h3>
        {d.kind !== 'other' && <span className={pill}>{d.kind}</span>}
        {!d.exported && <span className={pill}>not exported</span>}
        <span className="flex-1" />
        <Info choices={choices} />
      </div>
      {d.doc && <p className="m-0 text-[13px] whitespace-pre-line text-muted"><Inline text={d.doc} /></p>}
      {d.text && <code className="font-mono text-[12.5px] text-success [overflow-wrap:anywhere]">{d.rows.length ? 'includes ' : '= '}{code(d.text)}</code>}
      {d.rows.length > 0 && <SchemaTree rows={d.rows} whyAt={() => []} code={code} />}
      <Refs label="Uses" list={d.uses} names={names} />
      <Refs label="Used by" list={usedBy} names={names} />
    </article>
  );
}

/** A code contract of any language, once parsed into declarations: its types as a class diagram, then every declaration as a card. */
export function CodeContract({ model, source, choices }: { model: Model; source: string; choices: Choice[] }) {
  const names = new Set(model.decls.map((d) => d.name));
  const usedBy = (n: string) => model.decls.filter((d) => d.uses.includes(n)).map((d) => d.name);
  return (
    <div className="flex min-w-0 flex-col gap-4">
      <ClassDiagram decls={model.decls} onJump={jumpTo} />
      <section aria-label="Declarations" className="flex min-w-0 flex-col gap-5 rounded-[18px] border border-line bg-surface p-4">
        {model.imports.length > 0 && <p className="m-0 text-xs text-muted">Names from elsewhere: {model.imports.map((i) => `${i.names.join(', ')} (${i.from})`).join('; ')}</p>}
        {GROUPS.map(([title, kinds]) => {
          const ds = model.decls.filter((d) => kinds.includes(d.kind));
          return ds.length > 0 && (
            <div key={title} className="flex flex-col gap-3">
              <h2 className="m-0 text-[15px] font-semibold">{title} <span className="font-normal text-muted">{ds.length}</span></h2>
              {ds.map((d, i) => <Card key={`${d.name}-${i}`} d={d} names={names} usedBy={usedBy(d.name)} choices={choicesAt(choices, `/${d.name}`)} />)}
            </div>
          );
        })}
        <details className="border-t border-line pt-3">
          <summary className="cursor-pointer text-[13px] text-muted">Source</summary>
          <pre className="m-0 mt-3 overflow-x-auto rounded-xl border border-line bg-canvas p-3 font-mono text-xs leading-relaxed whitespace-pre text-ink-2">{source}</pre>
        </details>
      </section>
    </div>
  );
}

import type { ReactNode } from 'react';
import { Info } from './Info.tsx';
import type { Choice, Field } from './spec.ts';

const fact = 'inline-flex items-start gap-1 rounded bg-soft px-1.5 py-0.5 text-xs text-muted [overflow-wrap:anywhere]';

// A code type is source text: it takes the width it needs, and `code` links the declarations it names.
type Code = (type: string) => ReactNode;

function Row({ f, choices, code }: { f: Field; choices: Choice[]; code?: Code }) {
  return (
    <li style={{ paddingLeft: `${f.depth * 16}px` }} className="flex flex-wrap items-baseline gap-x-2 gap-y-1 border-t border-line py-2 text-[13.5px] first:border-t-0">
      <code className={`font-mono [overflow-wrap:anywhere] @xl:flex-none ${code ? '@xl:w-40' : '@xl:w-28'}`}>{f.name}</code>
      <span className={`font-mono text-[12.5px] text-success ${code ? 'min-w-0 [overflow-wrap:anywhere]' : '@xl:w-24 @xl:flex-none'}`}>{code ? code(f.type) : f.type}</span>
      {f.required && <span className="text-[11px] tracking-wide text-danger">REQUIRED</span>}
      {f.where && <span className={fact}>{f.where}</span>}
      <span className="flex min-w-0 flex-[1_1_8rem] flex-col gap-1 text-[13px] text-muted">
        {f.desc && <span>{f.desc}</span>}
        {f.facts.length > 0 && <span className="flex flex-wrap gap-1.5">{f.facts.map(([k, v]) => <span key={k} className={fact}><span className="flex-none whitespace-nowrap">{k}</span><code className="font-mono text-ink-2">{v}</code></span>)}</span>}
      </span>
      <Info choices={choices} />
    </li>
  );
}

/** Shared by every contract kind: one row per field with its type, required, default, example and an info button where choices govern it. */
export function SchemaTree({ rows, whyAt, code }: { rows: Field[]; whyAt: (ptr: string) => Choice[]; code?: Code }) {
  return <ul className="m-0 list-none p-0">{rows.map((f) => <Row key={f.ptr} f={f} choices={whyAt(f.ptr)} code={code} />)}</ul>;
}

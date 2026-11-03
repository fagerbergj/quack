import type { Choice, Field } from './spec.ts';

/** The "why" chip: opens the reasons behind one choice; 44 px to touch, tucked into the row's height. */
export function WhyChip({ choice, onWhy }: { choice: Choice; onWhy: (c: Choice) => void }) {
  return (
    <button type="button" aria-label={`Why: ${choice.decision}`} onClick={() => onWhy(choice)} className="-my-2 -mr-2 grid min-h-11 min-w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-xs font-semibold text-warning hover:bg-warning-bg">why</button>
  );
}

const fact = 'inline-flex items-start gap-1 rounded bg-soft px-1.5 py-0.5 text-xs text-muted [overflow-wrap:anywhere]';

function Row({ f, choice, onWhy }: { f: Field; choice?: Choice; onWhy: (c: Choice) => void }) {
  return (
    <li style={{ paddingLeft: `${f.depth * 16}px` }} className="flex flex-wrap items-baseline gap-x-2 gap-y-1 border-t border-line py-2 text-[13.5px] first:border-t-0">
      <code className="font-mono [overflow-wrap:anywhere] @xl:w-28 @xl:flex-none">{f.name}</code>
      <span className="font-mono text-[12.5px] text-success @xl:w-24 @xl:flex-none">{f.type}</span>
      {f.required && <span className="text-[11px] tracking-wide text-danger">REQUIRED</span>}
      {f.where && <span className={fact}>{f.where}</span>}
      <span className="flex min-w-0 flex-[1_1_8rem] flex-col gap-1 text-[13px] text-muted">
        {f.desc && <span>{f.desc}</span>}
        {f.facts.length > 0 && <span className="flex flex-wrap gap-1.5">{f.facts.map(([k, v]) => <span key={k} className={fact}><span className="flex-none whitespace-nowrap">{k}</span><code className="font-mono text-ink-2">{v}</code></span>)}</span>}
      </span>
      {choice && <WhyChip choice={choice} onWhy={onWhy} />}
    </li>
  );
}

/** Shared by every contract kind: one row per field with its type, required, default, example and a why chip where a choice governs it. */
export function SchemaTree({ rows, whyAt, onWhy }: { rows: Field[]; whyAt: (ptr: string) => Choice | undefined; onWhy: (c: Choice) => void }) {
  return <ul className="m-0 list-none p-0">{rows.map((f) => <Row key={f.ptr} f={f} choice={whyAt(f.ptr)} onWhy={onWhy} />)}</ul>;
}

import type { Change } from './diff.ts';
import { Info, Inline } from './Info.tsx';
import { MarkTag } from './SchemaTree.tsx';
import { BeforeAfter } from './Sections.tsx';
import type { Choice, ContractSpec } from './spec.ts';

const removedTag = 'flex-none rounded bg-danger-bg px-1.5 text-[10.5px] font-semibold tracking-wide text-danger uppercase';

// The decisions at a change's pointer, else at its nearest parent: a choice often governs a response or a type, the change one of its fields.
export function governing(choices: Choice[], ptr: string): Choice[] {
  const at = choices.filter((c) => c.pointer.length > 1 && (ptr === c.pointer || ptr.startsWith(`${c.pointer}/`)));
  const nearest = Math.max(...at.map((c) => c.pointer.length));
  return at.filter((c) => c.pointer.length === nearest);
}

// The agent's own account of a change is the secondary line, shown once under the first change its decision governs; the decision sits behind the info button.
function Item({ c, choices, why }: { c: Change; choices: Choice[]; why: string[] }) {
  return (
    <li className="flex flex-col gap-1.5 border-t border-line px-4 py-3 first:border-t-0">
      <div className="flex items-center gap-2">
        <code className="min-w-0 flex-1 font-mono text-[12px] text-muted [overflow-wrap:anywhere]">{c.crumb}</code>
        {c.mark === 'removed' ? <span className={removedTag}>Removed</span> : c.mark === 'added' && <MarkTag m="added" />}
        <Info choices={choices} />
      </div>
      {c.rows.map((r) => (
        <div key={r.name ?? ''} className="flex flex-col gap-1">
          {r.name && <span className="font-mono text-[12px] text-muted">{r.name}</span>}
          <BeforeAfter was={r.was} now={r.now} words />
        </div>
      ))}
      {why.map((w) => <p key={w} className="m-0 text-[12.5px] text-muted"><Inline text={w} /></p>)}
    </li>
  );
}

/** Every change between the two versions compared, each where it is and its text before and after. */
export function ChangeList({ changes, spec }: { changes: Change[]; spec: ContractSpec }) {
  const told = new Set<string>();
  if (!changes.length) return <p className="m-0 px-4 py-3 text-[13.5px] text-muted">No changes.</p>;
  return (
    <ul aria-label="What changed" className="m-0 list-none p-0">{changes.map((c, i) => {
      const choices = c.mark === 'removed' ? [] : governing(spec.choices, c.ptr);
      const fresh = choices.filter((ch) => !told.has(ch.id));
      fresh.forEach((ch) => told.add(ch.id));
      const why = spec.changes?.filter((x) => fresh.some((ch) => ch.id === x.choice)).map((x) => x.why) ?? [];
      return <Item key={`${c.ptr}${c.crumb}${i}`} c={c} choices={choices} why={why} />;
    })}</ul>
  );
}

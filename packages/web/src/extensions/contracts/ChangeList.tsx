import { useState } from 'react';
import { wordDiff } from '../../lineDiff.ts';
import type { Change } from './diff.ts';
import { Info, Inline } from './Info.tsx';
import { card } from './RestView.tsx';
import { MarkTag } from './SchemaTree.tsx';
import type { Choice, ContractSpec } from './spec.ts';

function Gap({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  if (open) return text;
  const n = text.match(/\S+/g)?.length ?? 0;
  return <><button type="button" aria-label={`Show ${n} unchanged words`} title="Show the unchanged words" onClick={() => setOpen(true)} className="cursor-pointer rounded border-0 bg-soft px-1.5 font-[inherit] text-muted hover:text-ink">…</button>{/\s$/.test(text) ? ' ' : ''}</>;
}

/** Two texts as one, word by word: removed words struck on the removed tint, added words on the added tint, long unchanged runs folded. */
export function Words({ was, now }: { was: string; now: string }) {
  return (
    <span className="whitespace-pre-wrap [overflow-wrap:anywhere]">
      {wordDiff(was, now).map((p, i, all) => {
        if (p.kind === 'gap') return <Gap key={i} text={p.text} />;
        // A replacement's struck and added runs touch; the margin keeps them apart.
        if (p.kind === 'del') return <del key={i} className={`rounded-sm bg-danger-bg text-ink-2 decoration-danger/70 ${all[i + 1]?.kind === 'add' ? 'mr-1' : ''}`}><span className="sr-only">Removed: </span>{p.text}</del>;
        if (p.kind === 'add') return <ins key={i} className="rounded-sm bg-success-bg text-ink no-underline"><span className="sr-only">Added: </span>{p.text}</ins>;
        return p.text;
      })}
    </span>
  );
}

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
        <p key={r.name ?? ''} className="m-0 text-sm leading-relaxed">
          {r.name && <span className="mr-2 font-mono text-[12px] text-muted">{r.name}</span>}
          <Words was={r.was} now={r.now} />
        </p>
      ))}
      {why.map((w) => <p key={w} className="m-0 text-[12.5px] text-muted"><Inline text={w} /></p>)}
    </li>
  );
}

/** Every change between the two versions compared, each where it is and its text before and after, under one count. */
export function ChangeList({ changes, spec, reason }: { changes: Change[]; spec: ContractSpec; reason?: string }) {
  const n = changes.length;
  const told = new Set<string>();
  return (
    <section aria-label="What changed" className="flex flex-col gap-2">
      <h3 className="m-0 font-sans text-[13px] font-normal text-muted">
        <span className="text-xs font-semibold tracking-wider text-ink-2 uppercase">{n ? `${n} ${n === 1 ? 'change' : 'changes'}` : 'No changes'}</span>
        {reason && ` since ${reason}`}
      </h3>
      {n > 0 && <ul className={`${card} m-0 list-none overflow-hidden p-0`}>{changes.map((c, i) => {
        const choices = c.mark === 'removed' ? [] : governing(spec.choices, c.ptr);
        const fresh = choices.filter((ch) => !told.has(ch.id));
        fresh.forEach((ch) => told.add(ch.id));
        const why = spec.changes?.filter((x) => fresh.some((ch) => ch.id === x.choice)).map((x) => x.why) ?? [];
        return <Item key={`${c.ptr}${c.crumb}${i}`} c={c} choices={choices} why={why} />;
      })}</ul>}
    </section>
  );
}

import { wordDiff, type Line } from '../../lineDiff.ts';
import { Inline } from './Info.tsx';

export const label = 'text-xs font-semibold uppercase tracking-wider text-muted';
export const card = 'rounded-[18px] border border-line bg-surface';

// One side of a before/after pair; with a word diff, the words that side alone has are tinted.
function Side({ title, parts, keep }: { title: string; parts: Line[]; keep: 'del' | 'add' }) {
  return (
    <div className="flex min-w-0 flex-col gap-1 rounded-xl border border-line px-3 py-2">
      <span className={label}>{title}</span>
      <p className="m-0 text-[13.5px] leading-relaxed whitespace-pre-wrap text-ink [overflow-wrap:anywhere]">
        {parts.map((p, i) => {
          // The exact wording is the point here, so unchanged runs are not folded away.
          if (p.kind === 'same' || p.kind === 'gap') return <Inline key={i} text={p.text} />;
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

/** A text before and after, as two blocks; `words` tints what each side alone has and puts a short value on one line. */
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

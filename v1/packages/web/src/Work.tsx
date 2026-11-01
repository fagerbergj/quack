import { useState } from 'react';
import type { SealedArtifact } from './api.ts';
import { Markdown } from './Markdown.tsx';

const tab = (on: boolean) =>
  `h-[28px] cursor-pointer rounded-full border-0 px-2.5 text-[12.5px] font-medium ${on ? 'bg-surface text-ink shadow-thumb' : 'bg-transparent text-muted hover:text-ink'}`;

function Pill({ met }: { met: number | null }) {
  const [text, tone] = met === null ? ['n/a', 'bg-soft text-muted'] : met >= 1 ? ['Pass', 'bg-success-bg text-success'] : ['Fail', 'bg-danger-bg text-danger'];
  return <span className={`rounded-full px-2 py-0.5 text-[11px] font-semibold ${tone}`}>{text}</span>;
}

function Checks({ a }: { a: SealedArtifact }) {
  if (!a.verdict) return <p className="m-0 text-[13px] text-muted">Not checked yet.</p>;
  return (
    <div className="flex flex-col gap-2 text-[13px]">
      <p className="m-0 font-semibold">{`${a.kind} v${a.version}`}</p>
      <ul className="m-0 flex list-none flex-col p-0">
        {a.verdict.checks.map((c) => (
          <li key={`${c.criterion}.${c.id}`} className="flex flex-col gap-1 border-b border-line py-2 last:border-0">
            <span className="flex items-center gap-2">
              <span className="min-w-0 flex-1 truncate font-mono text-[12px]">{`${c.criterion}.${c.id}`}</span>
              <Pill met={c.met} />
            </span>
            {c.notes.map((n, i) => <span key={i} className="text-[12.5px] text-ink-2">{n}</span>)}
          </li>
        ))}
      </ul>
    </div>
  );
}

// The newest sealed version, shown beside the conversation from xl up.
export function Work({ items }: { items: SealedArtifact[] }) {
  const [shown, setShown] = useState<'report' | 'checks'>('report');
  const a = items.reduce((best, x) => (x.sealedAt >= best.sealedAt ? x : best));
  return (
    <section aria-label="Work" className="hidden min-h-0 min-w-0 flex-col border-l border-line bg-soft/50 xl:flex">
      <div className="flex items-center border-b border-line px-5 py-2.5">
        <div role="tablist" aria-label="Session detail" className="inline-flex rounded-full border border-line bg-soft p-[3px]">
          <button role="tab" aria-selected={shown === 'report'} onClick={() => setShown('report')} className={tab(shown === 'report')}>Report</button>
          <button role="tab" aria-selected={shown === 'checks'} onClick={() => setShown('checks')} className={tab(shown === 'checks')}>Checks</button>
        </div>
      </div>
      <div className="flex-1 overflow-y-auto px-5 py-4">
        <div role="tabpanel" className="rounded-[22px] border border-line bg-surface p-5 text-sm leading-relaxed text-ink-2 shadow-card [overflow-wrap:anywhere]">
          {shown === 'report' ? (
            <>
              <p className="m-0 mb-3 flex items-center gap-2">
                <span className="rounded-md bg-soft px-2 py-0.5 font-mono text-xs text-ink-2">{a.kind}</span>
                <span className="text-xs text-muted">{`v${a.version}`}</span>
              </p>
              <Markdown text={a.content} />
            </>
          ) : (
            <Checks a={a} />
          )}
        </div>
      </div>
    </section>
  );
}

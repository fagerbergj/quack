import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
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

const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

// Modal drawer: Esc, the close button or the backdrop closes it, Tab stays inside, focus returns to the opener.
export function Work({ artifact: a, onClose }: { artifact: SealedArtifact; onClose: () => void }) {
  const [shown, setShown] = useState<'report' | 'checks'>('report');
  const panel = useRef<HTMLElement>(null);
  const closeBtn = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    closeBtn.current?.focus();
    return () => opener?.focus();
  }, []);
  const keys = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Escape') return onClose();
    if (e.key !== 'Tab') return;
    const f = [...(panel.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])];
    const end = e.shiftKey ? f[0] : f.at(-1);
    if (document.activeElement === end) {
      e.preventDefault();
      (e.shiftKey ? f.at(-1) : f[0])?.focus();
    }
  };
  return (
    <>
      <div aria-hidden="true" onClick={onClose} className="fixed inset-0 z-40 bg-ink/25 backdrop-blur-[2px]" />
      <section ref={panel} role="dialog" aria-modal="true" aria-label="Work" onKeyDown={keys} className="fixed inset-y-0 right-0 z-50 flex w-full flex-col border-l border-line bg-canvas shadow-lift sm:w-[min(560px,92vw)]">
        <div className="flex items-center gap-2 border-b border-line px-4 pt-[calc(0.5rem+env(safe-area-inset-top,0px))] pb-2 sm:px-5">
          <div role="tablist" aria-label="Session detail" className="inline-flex rounded-full border border-line bg-soft p-[3px]">
            <button role="tab" aria-selected={shown === 'report'} onClick={() => setShown('report')} className={tab(shown === 'report')}>Report</button>
            <button role="tab" aria-selected={shown === 'checks'} onClick={() => setShown('checks')} className={tab(shown === 'checks')}>Checks</button>
          </div>
          <button ref={closeBtn} type="button" aria-label="Close" onClick={onClose} className="ml-auto grid h-11 w-11 cursor-pointer place-items-center rounded-full text-ink-2 hover:bg-soft hover:text-ink">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
          </button>
        </div>
        <div className="flex-1 overflow-y-auto px-4 py-4 pb-[calc(1rem+env(safe-area-inset-bottom,0px))] sm:px-5">
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
    </>
  );
}

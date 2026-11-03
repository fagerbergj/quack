import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import type { SealedArtifact } from './api.ts';
import { artifactCard, artifactName } from './Artifacts.tsx';
import { SessionKeyContext } from './extension.ts';
import { delivered, versionOf } from './delivered.ts';
import { CheckRow } from './CheckList.tsx';
import { Content } from './JsonView.tsx';

const tab = (on: boolean) =>
  `h-11 lg:h-[28px] cursor-pointer rounded-full border-0 px-2.5 text-[12.5px] font-medium ${on ? 'bg-surface text-ink shadow-thumb' : 'bg-transparent text-muted hover:text-ink'}`;

function Checks({ a }: { a: SealedArtifact }) {
  if (!a.verdict) return <p className="m-0 text-[13px] text-muted">Its checks haven't finished.</p>;
  return (
    <div className="flex flex-col gap-2 text-[13px]">
      <ul className="m-0 flex list-none flex-col p-0">
        {a.verdict.checks.map((c) => (
          <li key={`${c.criterion}.${c.id}`} className="border-b border-line last:border-0"><CheckRow check={c} pill /></li>
        ))}
      </ul>
    </div>
  );
}

const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

// Modal drawer: Esc, the close button or the backdrop closes it, Tab stays inside, focus returns to the opener.
export function Work({ artifact, versions, session, onClose }: { artifact: SealedArtifact; versions?: SealedArtifact[]; session?: string; onClose: () => void }) {
  const [picked, setPicked] = useState(versionOf(artifact));
  const seen = versions && delivered(versions);
  const a = seen?.find((v) => versionOf(v) === picked) ?? artifact;
  const card = artifactCard(a);
  const [shown, setShown] = useState<'report' | 'checks'>('report');
  const id = useId();
  const arrows = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
    e.preventDefault();
    const next = shown === 'report' ? 'checks' : 'report';
    setShown(next);
    document.getElementById(`${id}-tab-${next}`)?.focus();
  };
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
    const f = [...(panel.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])].filter((el) => el.tabIndex >= 0);
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
          <div role="tablist" aria-label="Session detail" onKeyDown={arrows} className="inline-flex rounded-full border border-line bg-soft p-[3px]">
            {(['report', 'checks'] as const).map((t) => (
              <button key={t} id={`${id}-tab-${t}`} type="button" role="tab" aria-selected={shown === t} aria-controls={`${id}-panel`} tabIndex={shown === t ? 0 : -1} onClick={() => setShown(t)} className={tab(shown === t)}>{t === 'report' ? 'Report' : 'Checks'}</button>
            ))}
          </div>
          <button ref={closeBtn} type="button" aria-label="Close" onClick={onClose} className="ml-auto grid h-11 w-11 cursor-pointer place-items-center rounded-full text-ink-2 hover:bg-soft hover:text-ink">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
          </button>
        </div>
        <div className="flex-1 overflow-y-auto px-4 py-4 pb-[calc(1rem+var(--safe-b))] sm:px-5">
          {seen && seen.length > 1 && (
            <label className="mb-3 flex items-center gap-2 text-[13px] text-ink-2">
              Version
              <select value={picked} onChange={(e) => setPicked(Number(e.target.value))} className="h-11 rounded-full border border-line bg-soft px-3 text-[13px] outline-none lg:h-8">
                {seen.map((v) => <option key={v.version} value={versionOf(v)}>{`Version ${versionOf(v)}${v.verdict ? (v.verdict.passed ? ' · passed' : " · didn't pass") : ''}`}</option>)}
              </select>
            </label>
          )}
          <div role="tabpanel" id={`${id}-panel`} aria-labelledby={`${id}-tab-${shown}`} className="rounded-[22px] border border-line bg-surface p-5 text-sm leading-relaxed text-ink-2 shadow-card [overflow-wrap:anywhere]">
            {shown === 'report' ? (
              card ? <SessionKeyContext value={session}>{card}</SessionKeyContext> : <>
                <p className="m-0 mb-3 flex items-center gap-2">
                  <span className="rounded-md bg-soft px-2 py-0.5 text-xs text-ink-2">{artifactName(a)}</span>
                </p>
                <Content text={a.content} />
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

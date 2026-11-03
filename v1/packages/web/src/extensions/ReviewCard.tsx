import type { ReactNode } from 'react';
import type { SealedArtifact } from '../api.ts';
import { Markdown } from '../Markdown.tsx';
import { Verdict } from './sleeper/parts.tsx';
import { parseReview, type Finding, type Review } from './review.ts';

const VERDICT: Record<string, { label: string; tone: string; band: string }> = {
  approve: { label: 'Approved', tone: 'text-success', band: 'bg-success-bg' },
  request_changes: { label: 'Changes requested', tone: 'text-danger', band: 'bg-danger-bg' },
  comment: { label: 'Reviewed', tone: 'text-link', band: 'bg-link-bg' },
};
const SEVERITY: Record<string, string> = { blocking: 'bg-danger-bg text-danger', suggestion: 'bg-link-bg text-link', question: 'bg-warning-bg text-warning' };
const ORDER = ['blocking', 'suggestion', 'question', 'nit'];

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
const SUMMARY = 'flex min-h-11 cursor-pointer list-none [&::-webkit-details-marker]:hidden';
const summary = `${SUMMARY} items-center gap-1.5 text-[13px] text-muted hover:text-ink lg:min-h-8`;

function Chev({ className = '' }: { className?: string }) {
  return <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className={`chev flex-none transition-transform ${className}`}><path d="M9 6l6 6-6 6" /></svg>;
}

// Backtick spans in a one-line title read as code; nothing else is parsed.
function Inline({ text }: { text: string }) {
  return <>{text.split('`').map((p, i) => (i % 2 ? <code key={i} className="rounded bg-soft px-1 font-mono text-[12.5px]">{p}</code> : p))}</>;
}

function Seal({ className }: { className: string }) {
  return (
    <svg width="52" height="52" viewBox="0 0 52 52" aria-hidden="true" className={`flex-none -rotate-[8deg] ${className}`}>
      <circle cx="26" cy="26" r="24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeDasharray="4 3" />
      <circle cx="26" cy="26" r="18" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <g transform="translate(12 16) scale(.44)" fill="currentColor"><ellipse cx="34" cy="32" rx="22" ry="11" /><circle cx="20" cy="18" r="10" /><path d="M12 15 L1 19 L12 22 Z" /></g>
    </svg>
  );
}

function counts(findings: Finding[]): string[] {
  const by = Map.groupBy(findings, (f) => f.severity);
  const known = ORDER.filter((s) => by.has(s));
  return [...known, ...[...by.keys()].filter((s) => !ORDER.includes(s))].map((s) => {
    const n = by.get(s)!.length;
    return `${n} ${n > 1 && s !== 'blocking' ? `${s}s` : s}`;
  });
}

// A finding with more to say is itself the toggle; the chevron alone says it opens.
function FindingRow({ f }: { f: Finding }) {
  const head = (
    <span className="flex min-w-0 flex-1 flex-col gap-1.5">
      <span className={`inline-flex h-[22px] items-center self-start rounded-full px-2.5 text-[12px] font-semibold ${SEVERITY[f.severity] ?? 'bg-soft text-muted'}`}>{cap(f.severity) + (f.kind ? ` · ${f.kind}` : '')}</span>
      {f.loc && <code className="font-mono text-[12px] break-all text-link">{f.loc}</code>}
      <span className="text-[14px] leading-snug font-semibold"><Inline text={f.title} /></span>
    </span>
  );
  if (!f.more) return <div className="flex border-t border-line px-3.5 py-3">{head}</div>;
  return (
    <details className="border-t border-line">
      <summary className={`${SUMMARY} items-start gap-2 px-3.5 py-3 text-ink hover:bg-soft`}>{head}<Chev className="mt-1.5 text-muted" /></summary>
      <div className="px-3.5 pb-3 text-[13px] text-ink-2"><Markdown text={f.more} /></div>
    </details>
  );
}

function List({ label, items }: { label: string; items: string[] }) {
  if (!items.length) return null;
  return (
    <div className="border-t border-line px-3.5">
      <details>
        <summary className={summary}><span>{`${label} · ${items.length}`}</span><Chev /></summary>
        <div className="pb-2 text-[13px] text-ink-2"><Markdown text={items.map((i) => `- ${i}`).join('\n')} /></div>
      </details>
    </div>
  );
}

function Carried({ from }: { from: NonNullable<SealedArtifact['carriedFrom']> }) {
  const text = `Same change as the approved review of ${from.sha.slice(0, 8)}`;
  return from.url ? <a href={from.url} target="_blank" rel="noopener noreferrer" className="inline-flex min-h-11 items-center">{text}</a> : <span>{text}</span>;
}

function Footer({ a, md }: { a: SealedArtifact; md: string }) {
  return (
    <div className="border-t border-line px-3.5">
      {/* No verdict yet (the judge is still running) and nothing carried: no empty row. */}
      {(a.verdict || a.carriedFrom) && (
        <div className="flex min-h-11 items-center gap-2 border-b border-line text-[12.5px]">
          <Verdict artifact={a} />
          {a.carriedFrom && <Carried from={a.carriedFrom} />}
        </div>
      )}
      <details>
        <summary className={summary}><span>Full review</span><Chev /></summary>
        <div className="pb-3 text-[13.5px] text-ink-2"><Markdown text={md} /></div>
      </details>
    </div>
  );
}

function Body({ r, a, outdated }: { r: Review; a: SealedArtifact; outdated: boolean }): ReactNode {
  const v = VERDICT[r.verdict] ?? { label: cap(r.verdict.replace(/_/g, ' ')), tone: 'text-ink', band: 'bg-soft' };
  const posted = a.links?.[0];
  const line = counts(r.findings);
  return (
    <section aria-label="Review" className="overflow-hidden rounded-[20px] border border-line bg-surface shadow-card">
      <div className={`flex items-center gap-3 border-b border-line px-3.5 py-3.5 ${v.band}`}>
        <Seal className={v.tone} />
        <span className="flex min-w-0 flex-col gap-0.5">
          <span className={`font-display text-[18px] font-semibold ${v.tone}`}>{v.label}</span>
          <span className="text-[12.5px] text-ink-2">
            {line.length ? line.join(' · ') : 'No findings'}
            {posted && <>{' · '}<a href={posted.url} target="_blank" rel="noopener noreferrer" className="-my-3 inline-flex min-h-11 items-center">Posted to GitHub ↗</a></>}
          </span>
        </span>
      </div>
      {outdated && <p className="m-0 border-b border-line px-3.5 py-2 text-[12.5px] text-muted">About an earlier commit · reviewing the latest</p>}
      {r.takeaway && <div className="px-3.5 py-3 text-[14px] leading-normal text-ink [&_p]:m-0"><Markdown text={r.takeaway} /></div>}
      <div className={outdated ? 'opacity-60' : undefined}>{r.findings.map((f, i) => <FindingRow key={i} f={f} />)}</div>
      <List label="Verified" items={r.verified} />
      <List label="Notes" items={r.notes} />
      <Footer a={a} md={a.content} />
    </section>
  );
}

/** The review as a card; undefined for a sealed text that is not a review. */
export function reviewCard(a: SealedArtifact, outdated = false): ReactNode | undefined {
  const r = parseReview(a.content);
  return r && <Body r={r} a={a} outdated={outdated} />;
}

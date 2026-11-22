import type { SealedArtifact } from './api.ts';

/** The one status vocabulary: ✓ approved or passed, ✗ changes requested or failed, ! needs you, a dashed ring for a draft. */
export type Mark = 'ok' | 'bad' | 'need' | 'draft';

const MARKS: Record<Mark, [glyph: string, tone: string]> = {
  ok: ['✓', 'bg-success-bg text-success'],
  bad: ['✗', 'bg-danger-bg text-danger'],
  need: ['!', 'bg-warning-bg text-warning'],
  draft: ['', 'border-[1.5px] border-dashed border-muted'],
};
const box = 'grid h-4 w-4 flex-none place-items-center rounded-full text-[10px]';

// The status word is never on screen: it is the mark's tooltip and its accessible name.
export function StatusMark({ mark, label }: { mark: Mark; label: string }) {
  const [glyph, tone] = MARKS[mark];
  return <span role="img" aria-label={label} title={label} className={`${box} font-bold ${tone}`}>{glyph}</span>;
}

/** A version as every list shows it: vN, then its mark. */
export function VersionMark({ version, mark, label }: { version: number | undefined; mark: Mark; label: string }) {
  return (
    <span className="inline-flex flex-none items-center gap-1.5 text-[13px] text-ink-2 tabular-nums">
      {/* The trailing space collapses on screen but keeps "v3 Approved" apart in the accessible name. */}
      {version !== undefined && `v${version} `}
      <StatusMark mark={mark} label={label} />
    </span>
  );
}

// A pass is a tick with a tooltip, a fail an x; no verdict yet shows nothing. `label` lets a pass say more than "Double-checked".
export function StatusIcon({ verdict, label = 'Double-checked' }: { verdict: SealedArtifact['verdict']; label?: string }) {
  if (!verdict) return null;
  const [glyph, tone] = MARKS[verdict.passed ? 'ok' : 'bad'];
  return <span role={verdict.passed ? 'img' : undefined} aria-label={verdict.passed ? label : undefined} aria-hidden={verdict.passed ? undefined : true} title={verdict.passed ? label : undefined} className={`${box} ${tone}`}>{glyph}</span>;
}

import type { SealedArtifact } from './api.ts';

// A pass is a tick with a tooltip, a fail an x; no verdict yet shows nothing.
export function StatusIcon({ verdict }: { verdict: SealedArtifact['verdict'] }) {
  if (!verdict) return null;
  const [glyph, tone] = verdict.passed ? ['✓', 'bg-success-bg text-success'] : ['✗', 'bg-danger-bg text-danger'];
  return <span role={verdict.passed ? 'img' : undefined} aria-label={verdict.passed ? 'Double-checked' : undefined} aria-hidden={verdict.passed ? undefined : true} title={verdict.passed ? 'Double-checked' : undefined} className={`grid h-4 w-4 flex-none place-items-center rounded-full text-[10px] ${tone}`}>{glyph}</span>;
}

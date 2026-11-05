import type { SealedArtifact } from './api.ts';

// A pass is a tick with a tooltip, a fail an x; no verdict yet shows nothing. `label` lets a pass say more than "Double-checked".
export function StatusIcon({ verdict, label = 'Double-checked' }: { verdict: SealedArtifact['verdict']; label?: string }) {
  if (!verdict) return null;
  const [glyph, tone] = verdict.passed ? ['✓', 'bg-success-bg text-success'] : ['✗', 'bg-danger-bg text-danger'];
  return <span role={verdict.passed ? 'img' : undefined} aria-label={verdict.passed ? label : undefined} aria-hidden={verdict.passed ? undefined : true} title={verdict.passed ? label : undefined} className={`grid h-4 w-4 flex-none place-items-center rounded-full text-[10px] ${tone}`}>{glyph}</span>;
}

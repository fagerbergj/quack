import type { Spend } from './api.ts';
import { spendLabel } from './sessions.ts';

/** What a session cost, with the token breakdown on hover or long-press; nothing when nothing was spent. */
export function SpendChip({ spend, className }: { spend?: Spend; className?: string }) {
  const label = spendLabel(spend);
  return label ? <span title={label.title} className={`whitespace-nowrap tabular-nums ${className ?? ''}`}>{label.text}</span> : null;
}

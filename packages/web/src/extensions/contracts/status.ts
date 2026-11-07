import type { Row } from '../../sessions.ts';

export type Tone = 'warn' | 'plain' | 'bad';
export type Contract = { key: string; name: string; status: string; tone: Tone; rank: number };

/** What the Contracts page says about one contract session, from its catalog row; which version is approved is the extension's own record, not the row's. */
export function contractOf(row: Row): Contract {
  const base = { key: row.key, name: row.title ?? 'New contract' };
  if (row.pendingAsks) return { ...base, status: 'Waiting on you', tone: 'warn', rank: 0 };
  if (row.state === 'running') return { ...base, status: 'Drafting', tone: 'plain', rank: 1 };
  if (row.failures.length) return { ...base, status: 'Run failed', tone: 'bad', rank: 2 };
  return { ...base, status: 'Idle', tone: 'plain', rank: 3 };
}

export const contractRows = (rows: Row[]) => rows.filter((r) => r.key.startsWith('contracts:') && r.state !== 'archived').map(contractOf).sort((a, b) => a.rank - b.rank);

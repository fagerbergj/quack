import type { IndexArtifact } from '../../api.ts';
import type { Row } from '../../sessions.ts';

export type Tone = 'warn' | 'plain' | 'ok' | 'bad';
export type Contract = { key: string; name: string; kind: string; version?: number; status: string; tone: Tone; note?: string; rank: number };

const SPEC = /^(.*), version (\d+)$/;
const named = (row: Row, kind: string) => row.artifacts.filter((a) => a.kind === kind).sort((a, b) => a.version - b.version);
const outcomeOf = (a: IndexArtifact | undefined) => (a?.outcome ? Number(a.outcome.slice(1)) : undefined);

// The newest spec that passed its checks names the contract; the artifact index holds the title and version in its summary.
function latestSpec(row: Row) {
  const specs = named(row, 'contract.spec');
  const hit = specs.filter((a) => a.verdict?.passed).at(-1) ?? specs.at(-1);
  const m = hit?.summary && SPEC.exec(hit.summary);
  return { name: m ? m[1]! : (row.title ?? 'New contract'), version: m ? Number(m[2]) : undefined, passed: Boolean(hit?.verdict?.passed) };
}

/** What the Contracts page says about one contract session, from the index rows it already loaded. */
export function contractOf(row: Row): Contract {
  const { name, version, passed } = latestSpec(row);
  const approved = outcomeOf(named(row, 'contract.approved').at(-1));
  const asked = outcomeOf(named(row, 'contract.changes').at(-1));
  const base = { key: row.key, name, kind: 'REST', version };
  const stale = approved !== undefined && version !== undefined && version > approved ? `v${version} is not approved yet` : undefined;
  if (row.approvals.length) return { ...base, status: 'Awaiting your approval', tone: 'warn', rank: 0 };
  if (row.active || row.questions.length) return { ...base, status: 'Drafting', tone: 'plain', note: approved === undefined ? undefined : `v${approved} approved`, rank: 1 };
  if (asked !== undefined && asked === version && asked !== approved) return { ...base, status: 'Changes requested', tone: 'bad', rank: 2 };
  if (approved !== undefined) return { ...base, status: `Approved v${approved}`, tone: 'ok', note: stale, rank: 4 };
  return { ...base, status: 'Drafting', tone: 'plain', note: passed ? 'not yet proposed for approval' : undefined, rank: 3 };
}

export const contractRows = (rows: Row[]) => rows.filter((r) => r.key.startsWith('contracts:') && !r.archived).map(contractOf).sort((a, b) => a.rank - b.rank);

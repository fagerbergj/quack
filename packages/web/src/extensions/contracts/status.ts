import type { Row } from '../../sessions.ts';

export type Tone = 'warn' | 'plain' | 'ok' | 'bad';
export type Contract = { key: string; name: string; kind?: string; version?: number; status: string; tone: Tone; note?: string; rank: number };

/** One contract as the contracts extension serves it (GET apps/contracts/contracts), from the session's own sealed records. */
export type ContractRecord = { key: string; title: string; kind: string; draft?: number; proposed?: number; approved?: number; changesRequested?: number; pendingApproval: boolean };

const KIND_NAMES: Record<string, string> = { rest: 'REST', 'json-schema': 'JSON Schema', typescript: 'TypeScript' };

// Asked to change the very version last put up, and not approved since.
const changesAsked = (r: ContractRecord | undefined) => r?.changesRequested !== undefined && r.changesRequested === r.proposed && r.changesRequested !== r.approved;

function settled(base: Omit<Contract, 'status' | 'tone' | 'rank'>, rec: ContractRecord | undefined): Contract {
  if (changesAsked(rec)) return { ...base, status: 'Changes requested', tone: 'bad', rank: 2 };
  const stale = rec?.draft !== undefined && rec.approved !== undefined && rec.draft > rec.approved ? `v${rec.draft} is not approved yet` : undefined;
  if (rec?.approved !== undefined) return { ...base, status: `Approved v${rec.approved}`, tone: 'ok', ...(stale && { note: stale }), rank: 4 };
  return { ...base, status: rec?.draft !== undefined ? 'Drafting' : 'Idle', tone: 'plain', ...(rec?.draft !== undefined && { note: 'not yet proposed for approval' }), rank: 3 };
}

/** What the Contracts page says about one contract session: its catalog row for what runs, the extension's record for its versions. */
export function contractOf(row: Row, rec?: ContractRecord): Contract {
  const base = { key: row.key, name: rec?.title ?? row.title ?? 'New contract', ...(rec && { kind: KIND_NAMES[rec.kind] ?? 'REST' }), ...(rec?.draft !== undefined && { version: rec.draft }) };
  if (rec?.pendingApproval) return { ...base, status: 'Awaiting your approval', tone: 'warn', rank: 0 };
  if (row.pendingAsks) return { ...base, status: 'Waiting on you', tone: 'warn', rank: 0 };
  if (row.state === 'running') return { ...base, status: 'Drafting', tone: 'plain', ...(rec?.approved !== undefined && { note: `v${rec.approved} approved` }), rank: 1 };
  if (row.failures.length) return { ...base, status: 'Run failed', tone: 'bad', rank: 2 };
  return settled(base, rec);
}

export const contractRows = (rows: Row[], records: ReadonlyMap<string, ContractRecord> = new Map()) =>
  rows.filter((r) => r.key.startsWith('contracts:') && r.state !== 'archived').map((r) => contractOf(r, records.get(r.key))).sort((a, b) => a.rank - b.rank);

/** The contracts extension's records by session key; an app that is not mounted reads as none, and the rows still show. */
export async function contractRecords(): Promise<Map<string, ContractRecord>> {
  const res = await fetch('apps/contracts/contracts').catch(() => undefined);
  if (!res?.ok) return new Map();
  const body = (await res.json().catch(() => ({}))) as { contracts?: ContractRecord[] };
  return new Map((body.contracts ?? []).map((c) => [c.key, c]));
}

/** Where one sealed spec stands with the person, as the contracts extension reads it from the session's approval records and proposals. */
export type VersionStatus = 'draft' | 'proposed' | 'approved' | 'changes_requested';

/** Each spec artifact's status by artifact number (GET apps/contracts/contracts/{key}); undefined when the app answers not found or an error,
 * or the request fails, which Studio shows as no answer yet. */
export async function contractVersions(key: string): Promise<Map<number, VersionStatus> | undefined> {
  const res = await fetch(`apps/contracts/contracts/${encodeURIComponent(key)}`).catch(() => undefined);
  if (!res?.ok) return undefined;
  const body = (await res.json().catch(() => ({}))) as { versions?: Array<{ artifact: number; status: VersionStatus }> };
  return body.versions && new Map(body.versions.map((v) => [v.artifact, v.status]));
}

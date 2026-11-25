import { apiFetch, listAll, postInvocation, type Approval, type SealedArtifact } from '../../api.ts';
import type { Marking, Stand } from '../../extension.ts';
import type { Row } from '../../sessions.ts';
import type { Mark } from '../../StatusIcon.tsx';
import { verdictMark } from '../../VersionMenu.tsx';

/** One contract row: `version` and `mark` are all it shows of its state, and `status` is the mark's words. `approved` puts it in the Approved group. */
export type Contract = { key: string; name: string; kind?: { tag: string; name: string }; version: number | undefined; mark: Mark; status: string; approved: boolean; rank: number; at: number };

/** One contract as the contracts extension serves it (GET apps/contracts/contracts), from the session's own sealed records. */
export type ContractRecord = { key: string; title: string; kind: string; draft?: number; proposed?: number; approved?: number; changesRequested?: number; pendingApproval: boolean };

const KINDS: Record<string, { tag: string; name: string }> = { rest: { tag: 'REST', name: 'REST' }, 'json-schema': { tag: 'JSON', name: 'JSON Schema' }, typescript: { tag: 'TS', name: 'TypeScript' } };

// Asked to change the very version last put up, and not approved since.
const changesAsked = (r: ContractRecord | undefined) => r?.changesRequested !== undefined && r.changesRequested === r.proposed && r.changesRequested !== r.approved;

type Base = Omit<Contract, 'mark' | 'status' | 'rank'>;

function settled(base: Base, rec: ContractRecord | undefined): Contract {
  if (changesAsked(rec)) return { ...base, version: rec!.changesRequested, mark: 'bad', status: 'Changes requested', rank: 2 };
  if (rec?.approved !== undefined) {
    const newer = rec.draft !== undefined && rec.draft > rec.approved ? `; v${rec.draft} is not approved yet` : '';
    return { ...base, version: rec.approved, mark: 'ok', status: `Approved${newer}`, approved: true, rank: 4 };
  }
  return { ...base, mark: 'draft', status: rec?.draft !== undefined ? 'Draft, not yet proposed for approval' : 'No draft yet', rank: 3 };
}

/** What the Contracts page says about one contract session: its catalog row for what runs, the extension's record for its versions. */
export function contractOf(row: Row, rec?: ContractRecord): Contract {
  const base: Base = { key: row.key, name: rec?.title ?? row.title ?? 'New contract', ...(rec && { kind: KINDS[rec.kind] ?? KINDS.rest! }), version: rec?.draft, approved: false, at: Date.parse(row.lastActivity) || 0 };
  if (rec?.pendingApproval) return { ...base, version: rec.proposed ?? rec.draft, mark: 'need', status: 'Awaiting your approval', rank: 0 };
  if (row.pendingAsks) return { ...base, mark: 'need', status: 'Waiting on you', rank: 0 };
  if (row.checkingAsks) return { ...base, version: rec?.proposed ?? rec?.draft, mark: 'draft', status: 'Approving…', rank: 1 };
  if (row.state === 'running') return { ...base, mark: 'draft', status: 'Drafting', rank: 1 };
  if (row.failures.length) return { ...base, mark: 'bad', status: 'Run failed', rank: 2 };
  return settled(base, rec);
}

// Most urgent first, and within a rank (the Approved group is one rank) the most recently active.
export const contractRows = (rows: Row[], records: ReadonlyMap<string, ContractRecord> = new Map()) =>
  rows.filter((r) => r.key.startsWith('contracts:') && r.state !== 'archived').map((r) => contractOf(r, records.get(r.key))).sort((a, b) => a.rank - b.rank || b.at - a.at);

/** The contracts extension's records by session key, every page of them; an app that is not mounted reads as none, and the rows still show. */
export const contractRecords = async (): Promise<Map<string, ContractRecord>> =>
  new Map((await listAll<ContractRecord>('apps/contracts/contracts', 'contracts').catch(() => [])).map((c) => [c.key, c]));

/** Where one sealed spec stands with the person, as the contracts extension reads it from the session's approval records and proposals. */
export type VersionStatus = 'draft' | 'proposed' | 'approved' | 'changes_requested';

/** A version's standing as every version list draws it: one mark, its words spoken and in the tooltip. */
export const STANDING: Record<VersionStatus, Required<Marking>> = { draft: { mark: 'draft', status: 'draft' }, proposed: { mark: 'you', status: 'needs you' }, approved: { mark: 'ok', status: 'approved' }, changes_requested: { mark: 'bad', status: 'changes asked' } };

/** The sealed spec a proposal puts up for approval. */
export const offered = (p: Approval) => (p.tool === 'propose_contract' ? (p.args as { artifact?: number } | null)?.artifact : undefined);

/** Where a spec stands from the app's answer; the socket can show a proposal before the catalog has it. A draft the judge failed is marked failed. */
export const standOf = (statuses: Map<number, VersionStatus>) => (a: SealedArtifact, approvals: Approval[]): Stand => {
  const s = statuses.get(a.version) ?? 'draft';
  const state = s === 'draft' && approvals.some((p) => offered(p) === a.version) ? 'proposed' : s;
  // An approve being checked needs no one, so its version carries no needs-you mark.
  if (state === 'proposed' && approvals.some((p) => offered(p) === a.version && p.approving)) return { state, mark: 'draft', status: 'approving' };
  return { state, ...(state === 'draft' && a.verdict?.passed === false ? verdictMark(a.verdict) : STANDING[state]) };
};

/** Each spec artifact's status by artifact number (GET apps/contracts/contracts/{key}); undefined when the app answers not found or an error,
 * or the request fails, which Studio shows as no answer yet. */
export async function contractVersions(key: string): Promise<Map<number, VersionStatus> | undefined> {
  return (await readVersions(key))?.statuses;
}

/** Each spec artifact's version number by artifact number, from the same read; undefined when there is no answer. */
export async function specNumbers(key: string): Promise<Map<number, number> | undefined> {
  return (await readVersions(key))?.numbers;
}

async function readVersions(key: string) {
  const res = await apiFetch(`apps/contracts/contracts/${encodeURIComponent(key)}`).catch(() => undefined);
  if (!res?.ok) return undefined;
  const body = (await res.json().catch(() => ({}))) as { versions?: Array<{ artifact: number; version: number; status: VersionStatus }> };
  return body.versions && { statuses: new Map(body.versions.map((v) => [v.artifact, v.status])), numbers: new Map(body.versions.map((v) => [v.artifact, v.version])) };
}

// A working title from the description's first clause, cut at a word; the spec's own title takes over once it exists.
export function draftTitle(description: string): string {
  const first = description.replace(/\s+/g, ' ').trim().split(/(?<=[^\s]):\s|\.\s|\s\(/)[0]!;
  return first.length <= 60 ? first : `${first.slice(0, 60).replace(/\s+\S*$/, '')}…`;
}

// Starts the contracts agent on a description as the define_contract action does; the distributor creates the session on first use.
export async function startContract(description: string): Promise<string> {
  const id = crypto.randomUUID();
  const key = `contracts:${id}`;
  await postInvocation(key, 'contracts', { requestId: `ui-contract:${id}`, title: draftTitle(description), input: description });
  return key;
}

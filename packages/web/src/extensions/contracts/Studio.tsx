import { createElement, useEffect, useMemo, useRef, useState } from 'react';
import { checkFailed, decideApproval, type Approval, type SealedArtifact, type Verdict } from '../../api.ts';
import { ArtifactBoundary } from '../../ArtifactBoundary.tsx';
import type { StudioProps } from '../../extension.ts';
import { useLoad } from '../../useLoad.ts';
import { DiffContext, diffUnits, units, type Diff } from './diff.ts';
import { JsonSchemaView } from './JsonSchemaView.tsx';
import { Inline } from './Info.tsx';
import { card, label, RestView } from './RestView.tsx';
import { parseSpec, type ContractSpec } from './spec.ts';
import { contractVersions, type VersionStatus } from './status.ts';
import { TypeScriptView, useModel } from './TypeScriptView.tsx';

export const VIEWS = { rest: RestView, 'json-schema': JsonSchemaView, typescript: TypeScriptView };

/** One sealed spec artifact and the spec it holds. */
export type Version = { a: SealedArtifact; spec: ContractSpec };
/** Where a version stands with the person, from the contracts app. */
export type StatusOf = (v: Version) => VersionStatus;
const offered = (p: Approval) => (p.tool === 'propose_contract' ? (p.args as { artifact?: number } | null)?.artifact : undefined);

/** Each version's status from the contracts app, read again whenever a version is sealed, a proposal opens or is decided, or a run ends;
 * undefined until the first read answers. */
function useStatus(sessionKey: string, artifacts: SealedArtifact[], approvals: Approval[], running: boolean): StatusOf | undefined {
  const asked = approvals.map(offered).filter((n) => n !== undefined);
  const { data } = useLoad(() => contractVersions(sessionKey), [sessionKey, artifacts.length, asked.join(), running], undefined);
  if (!data) return undefined;
  // The socket can show a proposal before the catalog has it.
  return (v) => { const s = data.get(v.a.version) ?? 'draft'; return s === 'draft' && asked.includes(v.a.version) ? 'proposed' : s; };
}

type State = { can: boolean; done?: boolean; text: string };

// A version not up for approval, by its status; a verdict the socket carries only explains a draft the judge failed or is still checking.
function waiting(n: number, v: Verdict | null | undefined, status: VersionStatus | undefined, running: boolean): State {
  if (!status) return { can: false, text: `Reading where version ${n} stands…` };
  if (status === 'proposed') return { can: false, text: running ? `Version ${n} passed; the agent puts it up for approval when this step ends.` : `Version ${n} is not up for approval. Send a message and the agent puts it up again.` };
  if (v && !v.passed) {
    const failed = v.checks.filter(checkFailed).map((c) => c.label).join('; ');
    return { can: false, text: `Version ${n} can't be approved: it didn't pass its checks (${failed || v.gatesFailed.join(', ')}). ${v.limitReached ? 'Tell the agent what to change.' : 'The agent is revising it.'}` };
  }
  if (v === null) return { can: false, text: `Version ${n} can't be approved yet: its checks are still running.` };
  return { can: false, text: running ? `Version ${n} is a draft: the agent puts it up for approval once it passes its checks.` : `Version ${n} is a draft and not up for approval. Send a message and the agent goes on.` };
}

/** Whether the newest version can be approved now, and the reason shown either way. Only one the judge passed is ever put up for approval. */
export function approvalState(newest: Version | undefined, proposal: Approval | undefined, approved: number | undefined, running: boolean, status?: VersionStatus): State {
  if (!newest) return { can: false, text: 'Nothing to approve yet: the agent has not written a version.' };
  const n = newest.spec.version;
  if (approved === newest.a.version) return { can: false, done: true, text: `Version ${n} is approved.` };
  if (status === 'changes_requested') return { can: false, text: `You asked for changes to version ${n}.` };
  if (proposal) return { can: true, text: `Version ${n} passed its checks.` };
  return waiting(n, newest.a.verdict, status, running);
}

const SEEN = (key: string) => `quack.contracts.seen.${key}`;
function readSeen(key: string): number | undefined {
  try {
    const v = Number(localStorage.getItem(SEEN(key)));
    return Number.isInteger(v) && v > 0 ? v : undefined;
  } catch {
    return undefined;
  }
}

/** The version put to the person that this viewer looked at before the newest such one, by artifact number; it counts as looked at only while the pane is
 * `visible`. Kept in this browser, so it is per viewer and needs no server state; a lost store only means nothing is highlighted. */
export function useSeen(key: string, newest: number | undefined, visible: boolean): number | undefined {
  const [base, setBase] = useState(() => readSeen(key));
  const shown = useRef<number>(undefined);
  useEffect(() => {
    if (newest === undefined || !visible) return;
    if (shown.current !== undefined && shown.current !== newest) setBase(shown.current);
    shown.current = newest;
    try { localStorage.setItem(SEEN(key), String(newest)); } catch { /* the highlight is a convenience */ }
  }, [key, newest, visible]);
  return base === newest ? undefined : base;
}

const LABEL: Record<VersionStatus, string> = { draft: 'draft', proposed: 'awaiting approval', approved: 'approved', changes_requested: 'changes requested' };
const nameOf = (v: Version, all: Version[], status?: StatusOf) => `Version ${v.spec.version}${all.filter((x) => x.spec.version === v.spec.version).length > 1 ? ` (#${v.a.version})` : ''}${status ? `, ${LABEL[status(v)]}` : ''}`;

// A TypeScript spec is compared through its parsed declarations, which load with the compiler; until then there is no comparison.
function useDiff(from: Version | undefined, to: Version) {
  const was = useModel(from?.spec.kind === 'typescript' ? (from.spec.document as string) : '');
  const now = useModel(to.spec.kind === 'typescript' ? (to.spec.document as string) : '');
  return useMemo<Diff | undefined>(() => {
    if (!from || (from.spec.kind === 'typescript' && !was) || (to.spec.kind === 'typescript' && !now)) return undefined;
    return diffUnits(units(from.spec, was ?? undefined), units(to.spec, now ?? undefined));
  }, [from, to, was, now]);
}

const btn = 'min-h-11 cursor-pointer rounded-full border-0 bg-ink px-4 text-[14px] font-semibold text-on-ink hover:bg-ink-2 disabled:cursor-not-allowed disabled:opacity-50 lg:min-h-9';
type Break = { text: string };

function ApproveBar({ sessionKey, state, proposal, decided, breaks }: { sessionKey: string; state: State; proposal?: Approval; decided: (id: string) => void; breaks?: Break[] }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const approve = async () => {
    if (!proposal) return;
    setBusy(true);
    setError('');
    try {
      if (await decideApproval(sessionKey, proposal.id, 'approve')) decided(proposal.id);
      else setError('This version was already decided, in another tab or by a message you sent. The page shows where it stands now.');
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <section aria-label="Approval" className={`${card} sticky top-0 z-10 flex flex-col gap-2 px-4 py-3 shadow-card`}>
      <div className="flex flex-wrap items-center gap-3">
        <p role="status" className={`m-0 min-w-0 flex-1 basis-56 text-[13.5px] ${state.done ? 'font-semibold text-success' : 'text-ink-2'}`}>{state.text}</p>
        {!state.done && <button type="button" disabled={!state.can || busy} onClick={() => void approve()} className={btn}>{busy ? 'Approving…' : 'Approve'}</button>}
      </div>
      {state.can && breaks && <p className="m-0 text-[12.5px] text-muted">{breaks.length ? <Inline text={`Breaks callers of the approved version, checked in code: ${breaks.map((b) => b.text).join('; ')}`} /> : 'Checked in code: nothing breaks callers of the approved version.'}</p>}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
    </section>
  );
}

function Compare({ versions, status, from, to, auto, pick }: { versions: Version[]; status?: StatusOf; from?: number; to: number; auto?: string; pick: (p: { from?: number; to: number } | undefined) => void }) {
  const sel = 'h-11 min-w-0 rounded-full border border-line bg-soft px-3 text-[13px] outline-none lg:h-9';
  const opts = versions.map((v) => <option key={v.a.version} value={v.a.version}>{nameOf(v, versions, status)}</option>).reverse();
  return (
    <div className="flex flex-wrap items-center gap-2 text-[13px] text-ink-2">
      <label className="flex items-center gap-2">Changes since
        <select aria-label="Compare from" value={from ?? ''} onChange={(e) => pick({ from: e.target.value ? Number(e.target.value) : undefined, to })} className={sel}>
          <option value="">none</option>{opts}
        </select>
      </label>
      <label className="flex items-center gap-2">in
        <select aria-label="Compare to" value={to} onChange={(e) => pick({ from, to: Number(e.target.value) })} className={sel}>{opts}</select>
      </label>
      {auto !== undefined ? <span className="text-muted">{from ? auto : ''}</span> : <button type="button" onClick={() => pick(undefined)} className="min-h-11 cursor-pointer border-0 bg-transparent text-link lg:min-h-0">Follow the newest</button>}
    </div>
  );
}

function Summary({ diff, from }: { diff: Diff; from: Version }) {
  const count = (m: string) => [...diff.marks.values()].filter((x) => x === m).length;
  const parts = [[count('added'), 'new'], [count('changed'), 'changed'], [diff.removed.length, 'removed']].filter(([n]) => n).map(([n, w]) => `${n} ${w}`);
  return (
    <section aria-label="What changed" className={`${card} flex flex-col gap-1.5 px-4 py-3`}>
      <h3 className={`m-0 font-sans ${label}`}>{parts.length ? `${parts.join(' · ')} since version ${from.spec.version}` : `Nothing changed since version ${from.spec.version}`}</h3>
      {diff.removed.length > 0 && <ul className="m-0 flex list-none flex-col gap-1 p-0 text-[13px]">{diff.removed.map((r) => <li key={r} className="text-danger"><span aria-hidden="true">− </span><span className="sr-only">Removed </span>{r}</li>)}</ul>}
    </section>
  );
}

function Shown({ from, to }: { from?: Version; to: Version }) {
  const diff = useDiff(from, to);
  return (
    <>
      {from && diff && <Summary diff={diff} from={from} />}
      <DiffContext value={diff?.marks}>
        <ArtifactBoundary artifact={to.a}>{createElement(VIEWS[to.spec.kind], { key: to.a.version, spec: to.spec, full: true, verdict: to.a.verdict })}</ArtifactBoundary>
      </DiffContext>
    </>
  );
}

/** The contract as it stands, live beside the conversation: the newest draft with what changed since this viewer last looked, any two
 * versions on request, and one approve action for the newest version once the judge has passed it and it is up for approval. */
export function ContractStudio({ sessionKey, artifacts, approvals, running, decided, visible }: StudioProps) {
  const versions = useMemo(() => artifacts.filter((a) => a.kind === 'contract.spec').flatMap((a) => { const spec = parseSpec(a.content); return spec ? [{ a, spec }] : []; }), [artifacts]);
  const newest = versions.at(-1);
  const status = useStatus(sessionKey, artifacts, approvals, running);
  // A version counts once it was put to the person; until the status answers, none does and nothing is compared.
  const base = useSeen(sessionKey, status && versions.findLast((v) => status(v) !== 'draft')?.a.version, visible);
  const [picked, pick] = useState<{ from?: number; to: number }>();
  if (!newest) return <p className="m-0 p-5 text-sm text-muted">The contract shows here as soon as the agent writes its first version.</p>;
  const proposal = approvals.find((a) => offered(a) === newest.a.version);
  const approved = status && versions.findLast((v) => status(v) === 'approved')?.a.version;
  const state = approvalState(newest, proposal, approved, running, status?.(newest));
  const to = versions.find((v) => v.a.version === (picked?.to ?? newest.a.version)) ?? newest;
  // A viewer who has not looked yet is shown what changed since the approved version.
  const since = status && (base ?? approved);
  const from = versions.find((v) => v.a.version === (picked ? picked.from : since) && v !== to);
  return (
    <div className="mx-auto flex w-full max-w-[1100px] min-w-0 flex-col gap-4 px-4 pt-4 pb-10 sm:px-5">
      <ApproveBar sessionKey={sessionKey} state={state} proposal={proposal} decided={decided} breaks={approved === undefined ? undefined : (proposal?.args as { breaks?: Break[] } | null)?.breaks} />
      <Compare versions={versions} status={status} from={from?.a.version} to={to.a.version} auto={picked ? undefined : base ? 'the version you last looked at' : 'the approved version'} pick={pick} />
      <Shown from={from} to={to} />
    </div>
  );
}

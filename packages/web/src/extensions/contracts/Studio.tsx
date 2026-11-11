import { createElement, useMemo, useState } from 'react';
import { checkFailed, decideApproval, type Approval, type SealedArtifact, type Verdict } from '../../api.ts';
import { ArtifactBoundary } from '../../ArtifactBoundary.tsx';
import type { StudioProps } from '../../extension.ts';
import { useLoad } from '../../useLoad.ts';
import { FollowNewest, VersionMenu, type VersionItem } from '../../VersionMenu.tsx';
import { ChangeList } from './ChangeList.tsx';
import { Decided, Progress } from './Progress.tsx';
import { DiffContext, diffUnits, units, type Diff } from './diff.ts';
import { JsonSchemaView } from './JsonSchemaView.tsx';
import { Inline } from './Info.tsx';
import { card, RestView } from './RestView.tsx';
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

const LABEL: Record<VersionStatus, string> = { draft: 'draft', proposed: 'awaiting approval', approved: 'approved', changes_requested: 'changes requested' };
const nameOf = (v: Version, all: Version[]) => `Version ${v.spec.version}${all.filter((x) => x.spec.version === v.spec.version).length > 1 ? ` (#${v.a.version})` : ''}`;

// A TypeScript spec is compared through its parsed declarations, which load with the parser; until then there is no comparison.
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
        <p role="status" className="m-0 min-w-0 flex-1 basis-56 text-[13.5px] text-ink-2">{state.text}</p>
        <button type="button" disabled={!state.can || busy} onClick={() => void approve()} className={btn}>{busy ? 'Approving…' : 'Approve'}</button>
      </div>
      {state.can && breaks && <p className="m-0 text-[12.5px] text-muted">{breaks.length ? <Inline text={`Breaks callers of the approved version, checked in code: ${breaks.map((b) => b.text).join('; ')}`} /> : 'Checked in code: nothing breaks callers of the approved version.'}</p>}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
    </section>
  );
}

const TONE: Record<VersionStatus, [string, string]> = {
  draft: ['draft', 'bg-soft text-muted'], proposed: ['awaiting', 'bg-warning-bg text-warning'], approved: ['approved', 'bg-success-bg text-success'], changes_requested: ['changes asked', 'bg-danger-bg text-danger'],
};
const short = (v: Version, all: Version[]) => `v${v.spec.version}${all.filter((x) => x.spec.version === v.spec.version).length > 1 ? ` #${v.a.version}` : ''}`;
const itemsOf = (versions: Version[], status?: StatusOf): VersionItem[] => versions.map((v) => {
  const s = status?.(v);
  return { id: v.a.version, short: short(v, versions), name: nameOf(v, versions), status: s && LABEL[s], badge: s && { text: TONE[s][0], cls: TONE[s][1] } };
});

// The two versions compared on one row; once the person picks, an icon goes back to following the newest version.
function Compare({ versions, status, from, to, auto, pick }: { versions: Version[]; status?: StatusOf; from?: number; to: number; auto?: string; pick: (p: { from?: number; to: number } | undefined) => void }) {
  const items = itemsOf(versions, status);
  return (
    <div className="flex items-center gap-1.5">
      <VersionMenu name="Compare from" items={items} value={from} none="None: the whole contract" title={from && auto ? `Since ${auto}` : undefined} onPick={(f) => pick({ from: f, to })} />
      <span aria-hidden="true" className="text-muted">→</span>
      <VersionMenu name="Compare to" items={items} value={to} onPick={(t) => pick({ from, to: t ?? to })} />
      <span className="flex-1" />
      {auto === undefined && <FollowNewest onClick={() => pick(undefined)} />}
    </div>
  );
}

function Shown({ from, to, reason }: { from?: Version; to: Version; reason?: string }) {
  const diff = useDiff(from, to);
  return (
    <>
      {from && diff && <ChangeList changes={diff.changes} spec={to.spec} reason={reason} />}
      <DiffContext value={diff?.marks}>
        <ArtifactBoundary artifact={to.a}>{createElement(VIEWS[to.spec.kind], { key: to.a.version, spec: to.spec, verdict: to.a.verdict })}</ArtifactBoundary>
      </DiffContext>
    </>
  );
}

const layout = 'mx-auto flex w-full max-w-[1100px] min-w-0 flex-col gap-4 px-4 pt-4 pb-10 sm:px-5';

/** The contract as it stands, live beside the conversation: the newest draft with what changed since the approved version, any two
 * versions on request, and one approve action for the newest version once the judge has passed it and it is up for approval. */
export function ContractStudio({ sessionKey, artifacts, approvals, running, decided, questions, brief, activity }: StudioProps) {
  const versions = useMemo(() => artifacts.filter((a) => a.kind === 'contract.spec').flatMap((a) => { const spec = parseSpec(a.content); return spec ? [{ a, spec }] : []; }), [artifacts]);
  const newest = versions.at(-1);
  const status = useStatus(sessionKey, artifacts, approvals, running);
  const [picked, pick] = useState<{ from?: number; to: number }>();
  const decisions = <Decided sessionKey={sessionKey} asked={questions.length} running={running} open={!newest} />;
  if (!newest) return <div className={layout}><Progress activity={activity} brief={brief} waiting={questions.length > 0} />{decisions}</div>;
  const proposal = approvals.find((a) => offered(a) === newest.a.version);
  const approved = status && versions.findLast((v) => status(v) === 'approved')?.a.version;
  const state = approvalState(newest, proposal, approved, running, status?.(newest));
  const to = versions.find((v) => v.a.version === (picked?.to ?? newest.a.version)) ?? newest;
  // Until the status answers there is no approved version, so nothing is compared.
  const from = versions.find((v) => v.a.version === (picked ? picked.from : approved) && v !== to);
  const auto = picked ? undefined : 'the approved version';
  return (
    <div className={layout}>
      {!state.done && <ApproveBar sessionKey={sessionKey} state={state} proposal={proposal} decided={decided} breaks={approved === undefined ? undefined : (proposal?.args as { breaks?: Break[] } | null)?.breaks} />}
      <Compare versions={versions} status={status} from={from?.a.version} to={to.a.version} auto={auto} pick={pick} />
      <Shown from={from} to={to} reason={auto} />
      {decisions}
    </div>
  );
}

import { createElement, useEffect, useMemo, useState } from 'react';
import { checkFailed, listAsks, type Approval, type Ask, type SealedArtifact, type Verdict } from '../../api.ts';
import { ArtifactBoundary } from '../../ArtifactBoundary.tsx';
import type { StudioProps } from '../../extension.ts';
import { useLoad } from '../../useLoad.ts';
import { FollowNewest, VersionMenu, type VersionItem } from '../../VersionMenu.tsx';
import { Decision, type Break, type Pending } from './Approval.tsx';
import { ChangeList } from './ChangeList.tsx';
import { Decided, Progress } from './Progress.tsx';
import { DiffContext, diffUnits, units, type Diff } from './diff.ts';
import { JsonSchemaView } from './JsonSchemaView.tsx';
import { RestView } from './RestView.tsx';
import { card, Fold } from './Sections.tsx';
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

type State = { can: boolean; done?: boolean; text: string; detail?: string; asked?: boolean };
type Rejection = NonNullable<Ask['resolution']>;
const NO_ASKS: Ask[] = [];

/** Who turned down the newest version and why, from its answered proposal; read only while it stands at changes requested. */
function useRejection(sessionKey: string, artifact: number, on: boolean): Rejection | undefined {
  const { data } = useLoad(() => (on ? listAsks('tool:propose_contract state:resolved', sessionKey).catch(() => NO_ASKS) : Promise.resolve(NO_ASKS)), [sessionKey, artifact, on], NO_ASKS);
  return data.findLast((a) => a.resolution?.choice === 'reject' && Number((a.toolCall?.args as { artifact?: unknown } | null)?.artifact) === artifact)?.resolution;
}

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
export function approvalState(newest: Version | undefined, proposal: Approval | undefined, approved: number | undefined, running: boolean, status?: VersionStatus, rejection?: Rejection): State {
  if (!newest) return { can: false, text: 'Nothing to approve yet: the agent has not written a version.' };
  const n = newest.spec.version;
  if (approved === newest.a.version) return { can: false, done: true, text: `Version ${n} is approved.` };
  if (status === 'changes_requested') {
    const who = !rejection ? 'Someone' : rejection.by.kind === 'person' ? rejection.by.id : 'A reviewer';
    return { can: false, asked: true, text: `${who} asked for changes to version ${n}.`, detail: rejection?.reason?.split('\n').filter(Boolean).slice(0, 2).join('\n') };
  }
  if (proposal) return { can: true, text: `Version ${n} is up for your approval.` };
  return waiting(n, newest.a.verdict, status, running);
}

const LABEL: Record<VersionStatus, string> = { draft: 'draft', proposed: 'awaiting approval', approved: 'approved', changes_requested: 'changes requested' };
// A person knows a version by its number; when several seals share one, each is a draft of it, counted from 1.
const draftOf = (v: Version, all: Version[]) => { const same = all.filter((x) => x.spec.version === v.spec.version); return same.length > 1 ? same.indexOf(v) + 1 : undefined; };
const nameOf = (v: Version, all: Version[]) => `Version ${v.spec.version}${draftOf(v, all) ? `, draft ${draftOf(v, all)}` : ''}`;

// A TypeScript spec is compared through its parsed declarations, which load with the parser; until then there is no comparison.
function useDiff(from: Version | undefined, to: Version) {
  const was = useModel(from?.spec.kind === 'typescript' ? (from.spec.document as string) : '');
  const now = useModel(to.spec.kind === 'typescript' ? (to.spec.document as string) : '');
  return useMemo<Diff | undefined>(() => {
    if (!from || (from.spec.kind === 'typescript' && !was) || (to.spec.kind === 'typescript' && !now)) return undefined;
    return diffUnits(units(from.spec, was ?? undefined), units(to.spec, now ?? undefined));
  }, [from, to, was, now]);
}

/** The approval card: where the newest version stands and, when it is up for approval, what the person decides on. */
function ApproveBar({ sessionKey, state, proposal, decided, pending, stale }: { sessionKey: string; state: State; proposal?: Approval; decided: (id: string) => void; pending: Pending; stale?: string }) {
  return (
    <section aria-label="Approval" className={`${card} flex flex-col overflow-hidden`}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-3">
        <p role="status" className="m-0 min-w-0 flex-1 basis-56 text-[14px] font-semibold text-ink">{state.text}</p>
        {!state.can && !state.asked && <button type="button" disabled className="min-h-11 cursor-not-allowed rounded-full border-0 bg-ink px-4 text-[14px] font-semibold text-on-ink opacity-50 lg:min-h-9">Approve</button>}
        {state.detail && <p className="m-0 basis-full text-[13px] whitespace-pre-line text-muted">{state.detail}</p>}
        {stale && <p className="m-0 basis-full text-[13px] text-muted">{stale}</p>}
      </div>
      {state.can && proposal && <Decision sessionKey={sessionKey} proposal={proposal} pending={pending} decided={decided} />}
    </section>
  );
}

const TONE: Record<VersionStatus, [string, string]> = {
  draft: ['draft', 'bg-soft text-muted'], proposed: ['awaiting', 'bg-warning-bg text-warning'], approved: ['approved', 'bg-success-bg text-success'], changes_requested: ['changes asked', 'bg-danger-bg text-danger'],
};
const short = (v: Version, all: Version[]) => `v${v.spec.version}${draftOf(v, all) ? ` draft ${draftOf(v, all)}` : ''}`;
const itemsOf = (versions: Version[], status?: StatusOf): VersionItem[] => versions.map((v) => {
  const s = status?.(v);
  return { id: v.a.version, short: short(v, versions), name: nameOf(v, versions), note: draftOf(v, versions) ? `#${v.a.version}` : undefined, status: s && LABEL[s], badge: s && { text: TONE[s][0], cls: TONE[s][1] } };
});

// The two versions compared on one row; once the person picks, an icon goes back to following the newest version.
function Compare({ versions, status, from, to, auto, pick }: { versions: Version[]; status?: StatusOf; from?: number; to: number; auto?: string; pick: (p: { from?: number; to: number } | undefined) => void }) {
  const items = itemsOf(versions, status);
  return (
    <div className="flex items-center gap-0.5 text-[13px] text-muted">
      <span className="hidden min-[480px]:inline">Comparing</span>
      <VersionMenu quiet name="Compare from version" items={items} value={from} none="None: the whole contract" title={from && auto ? `Since ${auto}` : undefined} onPick={(f) => pick({ from: f, to })} />
      <span aria-hidden="true">→</span>
      <VersionMenu quiet name="Compare to version" items={items} value={to} onPick={(t) => pick({ from, to: t ?? to })} />
      <span className="flex-1" />
      {auto === undefined && <FollowNewest onClick={() => pick(undefined)} />}
    </div>
  );
}

// The word-level diff is computed only once its section opens, or when the person picked the two versions to compare.
function Shown({ from, to, versions, picked, told }: { from?: Version; to: Version; versions: Version[]; picked: boolean; told: boolean }) {
  const [want, setWant] = useState(picked);
  useEffect(() => void (picked && setWant(true)), [picked]);
  const diff = useDiff(want ? from : undefined, to);
  return (
    <>
      {from && (
        <Fold title={`Text diff from ${nameOf(from, versions).toLowerCase()}`} open={want} onToggle={setWant}>
          {diff ? <ChangeList changes={diff.changes} spec={to.spec} /> : <p className="m-0 px-4 py-3 text-[13.5px] text-muted">Comparing…</p>}
        </Fold>
      )}
      <DiffContext value={diff?.marks}>
        <ArtifactBoundary artifact={to.a}>{createElement(VIEWS[to.spec.kind], { key: to.a.version, spec: to.spec, verdict: to.a.verdict, told })}</ArtifactBoundary>
      </DiffContext>
    </>
  );
}

// A request still open for an older seal than the newest, as the person should read it.
function staleRequest(approvals: Approval[], versions: Version[], newest: Version): string | undefined {
  const was = versions.find((v) => v !== newest && approvals.some((a) => offered(a) === v.a.version));
  return was && `The approval request for ${nameOf(was, versions).toLowerCase()} is out of date: the agent has since written ${nameOf(newest, versions).toLowerCase()}.`;
}

const layout = 'mx-auto flex w-full max-w-[1100px] min-w-0 flex-col gap-4 px-4 pt-4 pb-10 sm:px-5';

/** The contract as it stands, live beside the conversation: the newest draft with what changed since the approved version, any two
 * versions on request, and one approve action for the newest version once the judge has passed it and it is up for approval. */
export function ContractStudio({ sessionKey, artifacts, approvals, running, decided, questions, brief, activity }: StudioProps) {
  const versions = useMemo(() => artifacts.filter((a) => a.kind === 'contract.spec').flatMap((a) => { const spec = parseSpec(a.content); return spec ? [{ a, spec }] : []; }), [artifacts]);
  const newest = versions.at(-1);
  const status = useStatus(sessionKey, artifacts, approvals, running);
  const rejected = useRejection(sessionKey, newest?.a.version ?? 0, !!newest && status?.(newest) === 'changes_requested');
  const [picked, pick] = useState<{ from?: number; to: number }>();
  const decisions = <Decided sessionKey={sessionKey} asked={questions.length} running={running} open={!newest} />;
  if (!newest) return <div className={layout}><Progress activity={activity} brief={brief} waiting={questions.length > 0} />{decisions}</div>;
  const proposal = approvals.find((a) => offered(a) === newest.a.version);
  const approved = status && versions.findLast((v) => status(v) === 'approved')?.a.version;
  const state = approvalState(newest, proposal, approved, running, status?.(newest), rejected);
  const to = versions.find((v) => v.a.version === (picked?.to ?? newest.a.version)) ?? newest;
  // Until the status answers there is no approved version, so nothing is compared.
  const from = versions.find((v) => v.a.version === (picked ? picked.from : approved) && v !== to);
  const auto = picked ? undefined : 'the approved version';
  const stale = staleRequest(approvals, versions, newest);
  const pending = { spec: newest.spec, verdict: newest.a.verdict, found: approved === undefined ? undefined : (proposal?.args as { breaks?: Break[] } | null)?.breaks };
  return (
    <div className={layout}>
      {(!state.done || stale) && <ApproveBar sessionKey={sessionKey} state={state} proposal={proposal} decided={decided} pending={pending} stale={stale} />}
      <Compare versions={versions} status={status} from={from?.a.version} to={to.a.version} auto={auto} pick={pick} />
      <Shown from={from} to={to} versions={versions} picked={!!picked} told={state.can && to === newest} />
      {decisions}
    </div>
  );
}

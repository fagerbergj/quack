import { createElement, useCallback, useMemo, useRef, useState, type ReactNode } from 'react';
import { checkFailed, listAsks, type Approval, type Ask, type SealedArtifact, type Verdict } from '../../api.ts';
import { ArtifactBoundary } from '../../ArtifactBoundary.tsx';
import { CheckResult } from '../../CheckResult.tsx';
import type { StudioProps } from '../../extension.ts';
import { ContentState, useContent, type Content } from '../../useContent.tsx';
import { useLoad } from '../../useLoad.ts';
import { FollowNewest, VersionMenu, type VersionItem } from '../../VersionMenu.tsx';
import { ApproveSheet, AskSheet, prim, sec, useDecide, type Break, type Pending } from './Approval.tsx';
import { CommentsProvider, DraftsLine, StrandedLine, useComments } from './Comments.tsx';
import { reasonText } from './comments.ts';
import { Decided, Progress } from './Progress.tsx';
import { DiffContext, diffUnits, units, type Diff } from './diff.ts';
import { JsonSchemaView } from './JsonSchemaView.tsx';
import { RestView } from './RestView.tsx';
import { ReviewContext, Sheet, Stepper, useStepper } from './Review.tsx';
import { changeItems, markBreaks, type Item } from './review.ts';
import { parseSpec, specVersion, type ContractSpec } from './spec.ts';
import { contractVersions, type VersionStatus } from './status.ts';
import { TypeScriptView, useModel } from './TypeScriptView.tsx';

export const VIEWS = { rest: RestView, 'json-schema': JsonSchemaView, typescript: TypeScriptView };

/** One version on its own, outside the studio: nothing compared, but the diagram and the linked names still jump within it. */
export function Standalone({ spec, verdict }: { spec: ContractSpec; verdict?: Verdict | null }) {
  return <ReviewContext value={useStepper(NO_ITEMS)}>{createElement(VIEWS[spec.kind], { spec, verdict })}</ReviewContext>;
}
const NO_ITEMS: Item[] = [];

/** One sealed spec artifact and the number a person knows it by. Its spec is read from its content, which only the newest version carries. */
export type Version = { a: SealedArtifact; n: number };
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

/** The status line: what is going on (`title`) and whether anything waits on the person (`sub`); `can` when the newest version is up for approval. */
export type State = { can: boolean; done?: boolean; title: string; sub?: string; detail?: string };
type Rejection = NonNullable<Ask['resolution']>;
const NO_ASKS: Ask[] = [];

/** Who turned down the newest version and why, from its answered proposal; read only while it stands at changes requested. */
function useRejection(sessionKey: string, artifact: number, on: boolean): Rejection | undefined {
  const { data } = useLoad(() => (on ? listAsks('tool:propose_contract state:resolved', sessionKey).catch(() => NO_ASKS) : Promise.resolve(NO_ASKS)), [sessionKey, artifact, on], NO_ASKS);
  return data.findLast((a) => a.resolution?.choice === 'reject' && Number((a.toolCall?.args as { artifact?: unknown } | null)?.artifact) === artifact)?.resolution;
}

/** What the status line is computed from: the newest version by its short name, and the approved one's. */
export type Standing = { name: string; verdict?: Verdict | null; status?: VersionStatus; proposal?: Approval; approved?: string; running: boolean; rejection?: Rejection };

// A version not up for approval, by its status; a verdict the socket carries only explains a draft the judge failed or is still checking.
function waiting({ name, verdict, status, running, approved }: Standing): State {
  const yet = approved ? ` · ${approved} is approved` : '';
  if (!status) return { can: false, title: `Reading where ${name} stands…` };
  if (status === 'proposed') return running ? { can: false, title: `${name} passed its checks`, sub: 'The agent puts it up for approval when this step ends · nothing waits on you yet' } : { can: false, title: `${name} is not up for approval`, sub: 'Send a message and the agent puts it up again' };
  if (verdict && !verdict.passed) {
    const failed = verdict.checks.filter(checkFailed).map((c) => c.label).join('; ') || verdict.gatesFailed.join(', ');
    return { can: false, title: `${name} failed its checks`, sub: verdict.limitReached ? 'Tell the agent what to change' : `The agent is revising it · nothing waits on you${yet}`, detail: `Failed: ${failed}` };
  }
  if (verdict === null) return { can: false, title: `Checking ${name}`, sub: `Nothing waits on you yet${yet}` };
  return running ? { can: false, title: `The agent is drafting ${name}`, sub: `Nothing waits on you${yet}` } : { can: false, title: `${name} is a draft`, sub: 'Not up for approval · send a message and the agent goes on' };
}

/** Where the newest version stands, as one status line for every state. Only a version the judge passed is ever put up for approval. */
export function approvalState(s: Standing & { isApproved: boolean }): State {
  const { name, running, rejection } = s;
  if (s.isApproved) return { can: false, done: true, title: `${name} is approved`, sub: running ? 'The agent is working · nothing waits on you' : 'Nothing in progress · nothing waits on you' };
  if (s.status === 'changes_requested') {
    const who = !rejection ? 'someone' : rejection.by.kind === 'person' ? rejection.by.id : 'a reviewer';
    return { can: false, title: `Changes asked for ${name}`, sub: `Asked by ${who}${running ? ' · the agent is revising it' : ''} · nothing waits on you`, detail: rejection?.reason && reasonText(rejection.reason).split('\n').filter(Boolean).slice(0, 2).join('\n') };
  }
  if (s.proposal) return { can: true, title: `${name} waits on you`, sub: 'Checks passed' };
  return waiting(s);
}

const LABEL: Record<VersionStatus, string> = { draft: 'draft', proposed: 'awaiting approval', approved: 'approved', changes_requested: 'changes requested' };
// A person knows a version by its number; when several seals share one, each is a draft of it, counted from 1.
const draftOf = (v: Version, all: Version[]) => { const same = all.filter((x) => x.n === v.n); return same.length > 1 ? same.indexOf(v) + 1 : undefined; };
const nameOf = (v: Version, all: Version[]) => `Version ${v.n}${draftOf(v, all) ? `, draft ${draftOf(v, all)}` : ''}`;
const short = (v: Version, all: Version[]) => `v${v.n}${draftOf(v, all) ? ` draft ${draftOf(v, all)}` : ''}`;

/** A version's spec: the newest is already in hand, an older one is loaded on demand (`wait` says how that stands). */
function useSpec(sessionKey: string, v: Version | undefined, newest: Version, spec: ContractSpec): { spec?: ContractSpec; wait: Content } {
  const own = v?.a === newest.a;
  const wait = useContent(sessionKey, own ? undefined : v?.a);
  const parsed = useMemo(() => (wait.content === undefined ? undefined : parseSpec(wait.content)), [wait.content]);
  return { spec: own ? spec : parsed, wait };
}

// A TypeScript spec is compared through its parsed declarations, which load with the parser; until then, or until the older version's content is there, there is no comparison.
function useDiff(from: ContractSpec | undefined, to: ContractSpec | undefined) {
  const was = useModel(from?.kind === 'typescript' ? (from.document as string) : '');
  const now = useModel(to?.kind === 'typescript' ? (to.document as string) : '');
  return useMemo<Diff | undefined>(() => {
    if (!from || !to || (from.kind === 'typescript' && !was) || (to.kind === 'typescript' && !now)) return undefined;
    return diffUnits(units(from, was ?? undefined), units(to, now ?? undefined));
  }, [from, to, was, now]);
}

const NO_BREAKS: Break[] = [];
function useItems(diff: Diff | undefined, spec: ContractSpec | undefined, found: Break[] = NO_BREAKS) {
  return useMemo(() => (diff && spec ? changeItems(diff, spec, found) : []), [diff, spec, found]);
}

/** The status line and, when the newest version waits on the person, Approve and Ask for changes (Review, with draft comments in hand); pinned to the bottom of the pane. */
function DecisionBar({ sessionKey, state, proposal, decided, pending, stale }: { sessionKey: string; state: State; proposal?: Approval; decided: (id: string) => void; pending: Pending; stale?: string }) {
  const [sheet, setSheet] = useState<'approve' | 'ask' | 'checks'>();
  const deciding = { ...useDecide(sessionKey, proposal, decided), onClose: () => setSheet(undefined) };
  const { drafts, open } = useComments();
  const checks = state.can && pending.verdict;
  const ask = drafts.length ? <button type="button" onClick={() => open({ kind: 'review' })} className={sec}>Review · {drafts.length}</button> : <button type="button" onClick={() => setSheet('ask')} className={sec}>Ask for changes</button>;
  return (
    <section aria-label="Approval" className="sticky bottom-0 z-10 -mx-4 mt-auto flex flex-col gap-1 border-t border-line bg-canvas/95 px-4 pt-2.5 pb-[max(0.625rem,var(--safe-b))] backdrop-blur sm:-mx-5 sm:px-5">
      <div className="flex items-center gap-2">
        <div className="flex min-w-0 flex-1 flex-col">
          <p role="status" className="m-0 text-[15px] leading-snug font-semibold text-ink [overflow-wrap:anywhere]">{state.title}</p>
          {checks ? <button type="button" onClick={() => setSheet('checks')} className="min-h-6 cursor-pointer self-start border-0 bg-transparent p-0 text-left text-[13px] whitespace-nowrap text-link underline-offset-2 hover:underline focus-ring">Checks passed</button>
            : state.sub && <p className="m-0 text-[13px] text-muted">{state.sub}</p>}
        </div>
        {state.can && <div className="flex flex-none gap-2">{ask}<button type="button" onClick={() => setSheet('approve')} className={prim}>Approve</button></div>}
      </div>
      {state.can && <DraftsLine />}
      <StrandedLine />
      {state.detail && <p className="m-0 max-h-24 overflow-y-auto text-[13px] whitespace-pre-line text-ink-2 [overflow-wrap:anywhere]">{state.detail}</p>}
      {stale && <p className="m-0 text-[13px] text-muted">{stale}</p>}
      {sheet === 'approve' && <ApproveSheet pending={pending} {...deciding} />}
      {sheet === 'ask' && <AskSheet name={pending.name} {...deciding} />}
      {sheet === 'checks' && pending.verdict && <Sheet title={`Automatic checks on ${pending.name}`} onClose={deciding.onClose}><CheckResult verdict={pending.verdict} open summary="Automatic checks passed" /></Sheet>}
    </section>
  );
}

const TONE: Record<VersionStatus, [string, string]> = {
  draft: ['draft', 'bg-soft text-muted'], proposed: ['awaiting', 'bg-warning-bg text-warning'], approved: ['approved', 'bg-success-bg text-success'], changes_requested: ['changes asked', 'bg-danger-bg text-danger'],
};
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

// The requests still open for older seals than the newest, as the person should read them: the newest of them, and how many.
function staleRequest(approvals: Approval[], versions: Version[], newest: Version): string | undefined {
  const stale = versions.filter((v) => v !== newest && approvals.some((a) => offered(a) === v.a.version));
  const was = stale.at(-1);
  const what = stale.length > 1 ? `${stale.length} approval requests are out of date, the newest for ${short(was!, versions)}` : `The approval request for ${was && short(was, versions)} is out of date`;
  return was && `${what}: the agent has since written ${short(newest, versions)}.`;
}

const layout = 'mx-auto flex min-h-full w-full max-w-[1100px] min-w-0 flex-col gap-5 px-4 pt-3 sm:px-5';

type Opened = { sessionKey: string; versions: Version[]; newest: Version; spec: ContractSpec; status?: StatusOf; approvals: Approval[]; running: boolean; decided: (id: string) => void; rejected?: Rejection; decisions: ReactNode };

/** A contract with at least one version: the changes against the approved version (or any two picked), the view, and the decision bar. */
function Contract({ sessionKey, versions, newest, spec, status, approvals, running, decided, rejected, decisions }: Opened) {
  const [picked, pick] = useState<{ from?: number; to: number }>();
  const top = useRef<HTMLDivElement>(null);
  const proposal = approvals.find((a) => offered(a) === newest.a.version);
  const approvedV = status && versions.findLast((v) => status(v) === 'approved');
  const to = versions.find((v) => v.a.version === (picked?.to ?? newest.a.version)) ?? newest;
  // Until the status answers there is no approved version, so nothing is compared.
  const base = approvedV !== newest ? approvedV : undefined;
  const from = picked ? versions.find((v) => v.a.version === picked.from && v !== to) : base;
  const found = approvedV ? (proposal?.args as { breaks?: Break[] } | null)?.breaks : undefined;
  const toSpec = useSpec(sessionKey, to, newest, spec), fromSpec = useSpec(sessionKey, from, newest, spec), baseSpec = useSpec(sessionKey, base, newest, spec);
  const diff = useDiff(fromSpec.spec, toSpec.spec), decisionDiff = useDiff(baseSpec.spec, spec);
  const items = useItems(diff, toSpec.spec, to === newest ? found : undefined);
  const decisionItems = useItems(decisionDiff, spec, found);
  const review = useStepper(items, from && short(from, versions), short(to, versions), true);
  const marks = useMemo(() => diff && markBreaks(diff.marks, items), [diff, items]);
  const name = short(newest, versions);
  const state = approvalState({ name, verdict: newest.a.verdict, status: status?.(newest), proposal, approved: approvedV && short(approvedV, versions), running, rejection: rejected, isApproved: !!approvedV && approvedV === newest });
  const pending: Pending = { spec, verdict: newest.a.verdict, found, items: decisionItems, base: base && short(base, versions), name };
  // Comments are made only on the version up for approval; a sent one's versions are named as the person knows them.
  const commentable = useMemo(() => (state.can && proposal ? { artifact: newest.a.version, name, proposal } : undefined), [state.can, proposal, newest.a.version, name]);
  const versionName = useCallback((n: number) => { const v = versions.find((x) => x.a.version === n); return v ? short(v, versions) : `#${n}`; }, [versions]);
  return (
    <CommentsProvider sessionKey={sessionKey} pending={commentable} newest={newest.a.version} versionName={versionName} decided={decided} deps={[versions.length, approvals.length, running]}>
    <div ref={top} className={layout}>
      <ReviewContext value={review}>
        {items.length > 0 && <Stepper onTop={() => top.current?.scrollIntoView?.({ block: 'start' })} />}
        <Compare versions={versions} status={status} from={from?.a.version} to={to.a.version} auto={picked ? undefined : 'the approved version'} pick={pick} />
        {from && <ContentState what={short(from, versions)} {...fromSpec.wait} />}
        <DiffContext value={marks}>
          {toSpec.spec ? <ArtifactBoundary artifact={to.a}>{createElement(VIEWS[toSpec.spec.kind], { key: to.a.version, spec: toSpec.spec, verdict: to.a.verdict })}</ArtifactBoundary>
            : <ContentState what={short(to, versions)} {...toSpec.wait} />}
        </DiffContext>
      </ReviewContext>
      {decisions}
      <DecisionBar sessionKey={sessionKey} state={state} proposal={proposal} decided={decided} pending={pending} stale={staleRequest(approvals, versions, newest)} />
    </div>
    </CommentsProvider>
  );
}

/** The contract as it stands, live beside the conversation: one status line for where it stands, the changes since the approved version
 * first, the map and the spec beneath it, and Approve or Ask for changes once the judge has passed the newest version and it is up for approval. */
export function ContractStudio({ sessionKey, artifacts, approvals, running, decided, questions, brief, activity }: StudioProps) {
  const versions = useMemo(() => artifacts.filter((a) => a.kind === 'contract.spec').map((a) => ({ a, n: specVersion(a) ?? a.version })), [artifacts]);
  const newest = versions.at(-1);
  // Only the newest version carries its content on the socket; one that cannot be drawn is no contract yet.
  const spec = useMemo(() => (newest?.a.content === undefined ? undefined : parseSpec(newest.a.content)), [newest?.a.content]);
  const status = useStatus(sessionKey, artifacts, approvals, running);
  const rejected = useRejection(sessionKey, newest?.a.version ?? 0, !!newest && status?.(newest) === 'changes_requested');
  const decisions = <Decided sessionKey={sessionKey} asked={questions.length} running={running} open={!newest || !spec} />;
  if (!newest || !spec) return <div className={`${layout} pb-10`}><Progress activity={activity} brief={brief} waiting={questions.length > 0} />{decisions}</div>;
  return <Contract sessionKey={sessionKey} versions={versions} newest={newest} spec={spec} status={status} approvals={approvals} running={running} decided={decided} rejected={rejected} decisions={decisions} />;
}

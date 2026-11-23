import { createElement, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';
import { checkFailed, listAsks, type Approval, type Ask, type SealedArtifact, type Verdict } from '../../api.ts';
import { ArtifactBoundary } from '../../ArtifactBoundary.tsx';
import { CheckResult } from '../../CheckResult.tsx';
import { SessionKeyContext, type StudioProps } from '../../extension.ts';
import { ContentState, useContent, type Content } from '../../useContent.tsx';
import { useLoad } from '../../useLoad.ts';
import { FollowNewest, iconButton, VersionMenu, type Mark, type VersionItem } from '../../VersionMenu.tsx';
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

/** An approval record drawn as the spec it approved: the record names that spec's sealed number and hash, and its content is read from the session. */
export function ApprovedSpec({ approval }: { approval: { artifact: number; hash: string; version: number } }) {
  const c = useContent(useContext(SessionKeyContext) ?? '', { kind: 'contract.spec', version: approval.artifact, hash: approval.hash, sealedAt: 0 });
  const spec = c.content === undefined ? undefined : parseSpec(c.content);
  if (!spec) return <ContentState what={`version ${approval.version}`} {...c} />;
  return (
    <>
      <p className="m-0"><span className="rounded-md bg-success-bg px-2 py-0.5 text-xs text-success">Approved</span></p>
      <Standalone spec={spec} />
    </>
  );
}

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

const short = (v: Version) => `v${v.n}`;

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

const MARK: Record<VersionStatus, [Mark, string]> = { draft: ['draft', 'draft'], proposed: ['you', 'needs you'], approved: ['ok', 'approved'], changes_requested: ['bad', 'changes asked'] };
const itemsOf = (versions: Version[], status?: StatusOf): VersionItem[] => versions.map((v) => {
  const s = status && MARK[status(v)];
  return { id: v.a.version, short: short(v), ...(s && { mark: s[0], status: s[1] }) };
});
// The person sees the versions they decided on and the newest; a save the checker sent back before it reached them is left out.
const listed = (versions: Version[], newest: Version, status?: StatusOf) => (status ? versions.filter((v) => v === newest || ['approved', 'changes_requested'].includes(status(v))) : versions);

const x = <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>;

// The shown version; Compare with… in its menu, or the approved version by default, adds the "since" chip, whose ✕ shows the version whole.
function Compare({ versions, status, from, to, picked, pick }: { versions: Version[]; status?: StatusOf; from?: number; to: number; picked: boolean; pick: (p: { from?: number; to: number } | undefined) => void }) {
  const items = itemsOf(versions, status);
  const older = items.slice(0, items.findIndex((i) => i.id === to));
  return (
    <div className="flex items-center gap-1.5">
      <VersionMenu name="Version" items={items} value={to} compare={(f) => pick({ from: f, to })} onPick={(t) => pick({ to: t })} />
      {from !== undefined && (
        <span className="flex items-center rounded-full border border-dashed border-line-strong">
          <VersionMenu since name="Compare with" items={older} value={from} onPick={(f) => pick({ from: f, to })} />
          <button type="button" aria-label="Stop comparing" title="Stop comparing" onClick={() => pick({ to })} className={`${iconButton} lg:h-9 lg:w-9`}>{x}</button>
        </span>
      )}
      <span className="flex-1" />
      {picked && <FollowNewest onClick={() => pick(undefined)} />}
    </div>
  );
}

// The requests still open for older seals than the newest, as the person should read them: the newest of them, and how many.
function staleRequest(approvals: Approval[], versions: Version[], newest: Version): string | undefined {
  const stale = versions.filter((v) => v !== newest && approvals.some((a) => offered(a) === v.a.version));
  const was = stale.at(-1);
  if (!was) return undefined;
  const what = stale.length > 1 ? `${stale.length} approval requests are out of date, the newest for ${short(was)}` : `The approval request for ${short(was)} is out of date`;
  return `${what}: the agent has since ${was.n === newest.n ? 'rewritten' : 'written'} ${short(newest)}.`;
}

const layout = 'mx-auto flex min-h-full w-full max-w-[1100px] min-w-0 flex-col gap-5 px-4 pt-3 sm:px-5';

type Opened = { sessionKey: string; versions: Version[]; newest: Version; spec: ContractSpec; status?: StatusOf; approvals: Approval[]; running: boolean; decided: (id: string) => void; rejected?: Rejection; decisions: ReactNode };

/** A contract with at least one version: the changes against the approved version (or any two picked), the view, and the decision bar. */
function Contract({ sessionKey, versions: all, newest, spec, status, approvals, running, decided, rejected, decisions }: Opened) {
  const [picked, pick] = useState<{ from?: number; to: number }>();
  const versions = useMemo(() => listed(all, newest, status), [all, newest, status]);
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
  const review = useStepper(items, from && short(from), short(to), true);
  const marks = useMemo(() => diff && markBreaks(diff.marks, items), [diff, items]);
  const name = short(newest);
  const state = approvalState({ name, verdict: newest.a.verdict, status: status?.(newest), proposal, approved: approvedV && short(approvedV), running, rejection: rejected, isApproved: !!approvedV && approvedV === newest });
  const pending: Pending = { spec, verdict: newest.a.verdict, found, items: decisionItems, base: base && short(base), name };
  // Comments are made only on the version up for approval; a sent one's versions are named as the person knows them.
  const commentable = useMemo(() => (state.can && proposal ? { artifact: newest.a.version, name, proposal } : undefined), [state.can, proposal, newest.a.version, name]);
  const versionName = useCallback((n: number) => { const v = all.find((x) => x.a.version === n); return v ? short(v) : `#${n}`; }, [all]);
  return (
    <CommentsProvider sessionKey={sessionKey} pending={commentable} newest={newest.a.version} viewing={to.a.version} versionName={versionName} decided={decided} deps={[versions.length, approvals.length, running]}>
    <div ref={top} className={layout}>
      <ReviewContext value={review}>
        {items.length > 0 && <Stepper onTop={() => top.current?.scrollIntoView?.({ block: 'start' })} />}
        <Compare versions={versions} status={status} from={from?.a.version} to={to.a.version} picked={!!picked} pick={pick} />
        {from && <ContentState what={short(from)} {...fromSpec.wait} />}
        <DiffContext value={marks}>
          {toSpec.spec ? <ArtifactBoundary artifact={to.a}>{createElement(VIEWS[toSpec.spec.kind], { key: to.a.version, spec: toSpec.spec, verdict: to.a.verdict })}</ArtifactBoundary>
            : <ContentState what={short(to)} {...toSpec.wait} />}
        </DiffContext>
      </ReviewContext>
      {decisions}
      <DecisionBar sessionKey={sessionKey} state={state} proposal={proposal} decided={decided} pending={pending} stale={staleRequest(approvals, all, newest)} />
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

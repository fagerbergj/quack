import { createElement, useEffect, useMemo, useRef, useState } from 'react';
import { checkFailed, decideApproval, type Approval, type SealedArtifact, type Verdict } from '../../api.ts';
import { ArtifactBoundary } from '../../ArtifactBoundary.tsx';
import type { StudioProps } from '../../extension.ts';
import { useLoad } from '../../useLoad.ts';
import { useMenu } from '../../useMenu.ts';
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

function Badge({ s }: { s: VersionStatus }) {
  return <span aria-hidden="true" className={`rounded px-1.5 text-[10.5px] font-semibold tracking-wide whitespace-nowrap uppercase ${TONE[s][1]}`}>{TONE[s][0]}</span>;
}

const item = 'flex min-h-11 w-full cursor-pointer items-center gap-2 border-0 bg-transparent px-3 text-left text-[13px] text-ink hover:bg-soft aria-[current=true]:font-semibold lg:min-h-9';
const chevron = <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6" /></svg>;

// One side of the comparison: a version and its status, a menu of every version to pick another; `none` offers the whole contract instead.
function Side({ name, versions, status, value, none, title, onPick }: { name: string; versions: Version[]; status?: StatusOf; value?: number; none?: string; title?: string; onPick: (v?: number) => void }) {
  const menu = useMenu();
  const v = versions.find((x) => x.a.version === value);
  const choose = (n?: number) => { menu.close(); onPick(n); };
  return (
    <div ref={menu.root} onKeyDown={menu.keys} className="relative">
      <button ref={menu.button} type="button" aria-haspopup="menu" aria-expanded={menu.open} aria-label={`${name}: ${v ? nameOf(v, versions, status) : none}`} title={title} onClick={menu.toggle} className="flex h-11 cursor-pointer items-center gap-1.5 rounded-full border border-line bg-soft px-3 text-[13px] text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link">
        <span className="font-mono font-semibold whitespace-nowrap">{v ? short(v, versions) : 'none'}</span>{v && status && <Badge s={status(v)} />}{chevron}
      </button>
      {menu.open && (
        <div role="menu" aria-label={name} className="absolute left-0 z-20 mt-1 min-w-60 rounded-xl border border-line-strong bg-surface py-1 shadow-float">
          {none && <button type="button" role="menuitem" aria-current={!v || undefined} tabIndex={-1} onClick={() => choose(undefined)} className={item}>{none}</button>}
          {[...versions].reverse().map((x) => (
            <button key={x.a.version} type="button" role="menuitem" aria-current={x === v || undefined} tabIndex={-1} onClick={() => choose(x.a.version)} className={item}>
              <span className="flex-1">{nameOf(x, versions)}</span>{status && <Badge s={status(x)} />}<span className="sr-only">{status && `, ${LABEL[status(x)]}`}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// The two versions compared on one row; once the person picks, an icon goes back to following the newest version.
function Compare({ versions, status, from, to, auto, pick }: { versions: Version[]; status?: StatusOf; from?: number; to: number; auto?: string; pick: (p: { from?: number; to: number } | undefined) => void }) {
  return (
    <div className="flex items-center gap-1.5">
      <Side name="Compare from" versions={versions} status={status} value={from} none="None: the whole contract" title={from && auto ? `Since ${auto}` : undefined} onPick={(f) => pick({ from: f, to })} />
      <span aria-hidden="true" className="text-muted">→</span>
      <Side name="Compare to" versions={versions} status={status} value={to} onPick={(t) => pick({ from, to: t ?? to })} />
      <span className="flex-1" />
      {auto === undefined && (
        <button type="button" aria-label="Follow the newest version" title="Follow the newest version" onClick={() => pick(undefined)} className="grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 hover:bg-soft hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5" /></svg>
        </button>
      )}
    </div>
  );
}

function Shown({ from, to, reason }: { from?: Version; to: Version; reason?: string }) {
  const diff = useDiff(from, to);
  return (
    <>
      {from && diff && <ChangeList changes={diff.changes} spec={to.spec} reason={reason} />}
      <DiffContext value={diff?.marks}>
        <ArtifactBoundary artifact={to.a}>{createElement(VIEWS[to.spec.kind], { key: to.a.version, spec: to.spec, full: true, verdict: to.a.verdict })}</ArtifactBoundary>
      </DiffContext>
    </>
  );
}

const layout = 'mx-auto flex w-full max-w-[1100px] min-w-0 flex-col gap-4 px-4 pt-4 pb-10 sm:px-5';

/** The contract as it stands, live beside the conversation: the newest draft with what changed since this viewer last looked, any two
 * versions on request, and one approve action for the newest version once the judge has passed it and it is up for approval. */
export function ContractStudio({ sessionKey, artifacts, approvals, running, decided, visible, questions, brief, activity }: StudioProps) {
  const versions = useMemo(() => artifacts.filter((a) => a.kind === 'contract.spec').flatMap((a) => { const spec = parseSpec(a.content); return spec ? [{ a, spec }] : []; }), [artifacts]);
  const newest = versions.at(-1);
  const status = useStatus(sessionKey, artifacts, approvals, running);
  // A version counts once it was put to the person; until the status answers, none does and nothing is compared.
  const base = useSeen(sessionKey, status && versions.findLast((v) => status(v) !== 'draft')?.a.version, visible);
  const [picked, pick] = useState<{ from?: number; to: number }>();
  const decisions = <Decided sessionKey={sessionKey} asked={questions.length} running={running} open={!newest} />;
  if (!newest) return <div className={layout}><Progress activity={activity} brief={brief} waiting={questions.length > 0} />{decisions}</div>;
  const proposal = approvals.find((a) => offered(a) === newest.a.version);
  const approved = status && versions.findLast((v) => status(v) === 'approved')?.a.version;
  const state = approvalState(newest, proposal, approved, running, status?.(newest));
  const to = versions.find((v) => v.a.version === (picked?.to ?? newest.a.version)) ?? newest;
  // A viewer who has not looked yet is shown what changed since the approved version.
  const since = status && (base ?? approved);
  const from = versions.find((v) => v.a.version === (picked ? picked.from : since) && v !== to);
  const auto = picked ? undefined : base ? 'the version you last looked at' : 'the approved version';
  return (
    <div className={layout}>
      {!state.done && <ApproveBar sessionKey={sessionKey} state={state} proposal={proposal} decided={decided} breaks={approved === undefined ? undefined : (proposal?.args as { breaks?: Break[] } | null)?.breaks} />}
      <Compare versions={versions} status={status} from={from?.a.version} to={to.a.version} auto={auto} pick={pick} />
      <Shown from={from} to={to} reason={auto} />
      {decisions}
    </div>
  );
}

import { useState, type ReactNode } from 'react';
import { listAgents, type AgentSummary, type Approval, type Question, type SealedArtifact, type Spend } from './api.ts';
import { artifactName, ledger, ledgerId } from './Artifacts.tsx';
import { versionOf } from './delivered.ts';
import { describe } from './Needs.tsx';
import { compact, needId } from './sessions.ts';
import { Dialog } from './Dialog.tsx';
import { useLoad } from './useLoad.ts';
import type { View } from './view.ts';
import { useMedia, WIDE } from './useDesktop.ts';
import { RailSections } from './slots.tsx';
import { EXTENSIONS } from './extensions/index.ts';

const head = 'm-0 mb-3 text-xs font-semibold tracking-[.06em] text-muted uppercase';
const jumpBtn = 'flex min-h-11 w-full cursor-pointer items-start border-0 bg-transparent p-0 text-left text-[13px] text-ink lg:min-h-8';

function jump(id: string) {
  document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' });
}

const clock = (at: string | number) => new Date(at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });

// A score only when partial, as in the ledger's check result.
function summaryOf(a: SealedArtifact): string {
  const v = a.verdict;
  const pct = v && v.score > 0 && v.score < 1 ? `, ${Math.round(v.score * 100)}%` : '';
  const checked = v ? [`${v.passed ? 'Double-checked' : "Didn't pass its checks"}${pct}`] : [];
  const postedTo = EXTENSIONS.map((e) => e.artifact?.(a, false)).find(Boolean)?.postedTo;
  const posted = a.links?.length ? [postedTo ?? 'Posted'] : [];
  return [...checked, ...posted].join(' · ');
}

function Versions({ items }: { items: SealedArtifact[] }) {
  const kind = items.reduce<SealedArtifact | undefined>((n, a) => (!n || a.sealedAt >= n.sealedAt ? a : n), undefined)?.kind;
  const rows = ledger(items).filter(({ a }) => a.kind === kind);
  if (!rows.length) return null;
  return (
    <section aria-labelledby="rail-versions">
      <h2 id="rail-versions" className={head}>Versions</h2>
      <ul className="m-0 flex list-none flex-col gap-1 p-0">
        {rows.map(({ a }) => (
          <li key={ledgerId(a)}>
            <button type="button" onClick={() => jump(ledgerId(a))} className={`${jumpBtn} flex-col gap-0.5`}>
              <span className="flex w-full items-center gap-2">
                {a.verdict && <span aria-hidden="true" className={a.verdict.passed ? 'text-success' : 'text-danger'}>{a.verdict.passed ? '✓' : '✗'}</span>}
                <span className="min-w-0 flex-1 truncate font-medium">{`${artifactName(a)} v${versionOf(a)}`}</span>
                <time dateTime={new Date(a.sealedAt).toISOString()} className="flex-none text-[12px] text-muted">{clock(a.sealedAt)}</time>
              </span>
              {summaryOf(a) && <span className="text-[12px] text-muted">{summaryOf(a)}</span>}
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function NeedsYou({ approvals, questions }: { approvals: Approval[]; questions: Question[] }) {
  const lines = [
    ...questions.map((q) => ({ id: q.id, text: q.title ?? q.question })),
    ...approvals.map((a) => ({ id: a.id, text: describe(a.args) ?? a.tool })),
  ];
  return (
    <section aria-labelledby="rail-needs">
      <h2 id="rail-needs" className={head}>Needs you</h2>
      {lines.length === 0 ? <p className="m-0 text-[13px] text-muted">Nothing is waiting on you.</p> : (
        <ul className="m-0 flex list-none flex-col gap-1 p-0">
          {lines.map((l) => <li key={l.id}><button type="button" onClick={() => jump(needId(l.id))} className={`${jumpBtn} text-warning`}>{l.text}</button></li>)}
        </ul>
      )}
    </section>
  );
}

function Cost({ spend }: { spend?: Spend }) {
  if (!spend || (spend.cost <= 0 && spend.tokens.input + spend.tokens.output === 0)) return null;
  const { input, output, cacheRead } = spend.tokens;
  return (
    <section aria-labelledby="rail-cost">
      <h2 id="rail-cost" className={head}>Cost</h2>
      {spend.cost > 0 && (
        <p className="m-0 mb-3 flex items-baseline gap-2">
          <span className="text-[28px] leading-none font-semibold tabular-nums">{spend.cost < 0.01 ? '<$0.01' : `$${spend.cost.toFixed(2)}`}</span>
          <span className="text-[12px] text-muted">at today's prices</span>
        </p>
      )}
      <dl className="m-0 grid grid-cols-3 gap-3 text-[13px]">
        {([['Input', input], ['Output', output], ['Cached', cacheRead]] as const).map(([name, n]) => (
          <div key={name} className="flex flex-col-reverse">
            <dd className="m-0 font-medium tabular-nums">{compact(n)}</dd>
            <dt className="text-[12px] text-muted">{name}</dt>
          </div>
        ))}
      </dl>
    </section>
  );
}

const NO_AGENTS: AgentSummary[] = [];

function SessionInfo({ agent, created, activity }: { agent?: string; created?: string; activity?: string }) {
  const { data: agents } = useLoad(listAgents, [], NO_AGENTS);
  const model = agents.find((a) => a.name === agent)?.model;
  if (!agent && !created) return null;
  return (
    <section aria-labelledby="rail-session" className="border-t border-line pt-7">
      <h2 id="rail-session" className={head}>Session</h2>
      <div className="flex flex-col gap-1.5 text-[13px] text-muted">
        {agent && <p className="m-0">{`Agent ${agent}${model ? ` on ${model}` : ''}`}</p>}
        {created && <p className="m-0">{`Started ${clock(created)}${activity ? ` · Last activity ${clock(activity)}` : ''}`}</p>}
      </div>
    </section>
  );
}

// The trigger text of a review is the first user message; the rail reads the pull request's branches from it.
function openingOf(view: View): string | undefined {
  for (const it of view.items) {
    if (it.kind !== 'entry') continue;
    const m = (it.entry.model ?? []).find((x) => x.role === 'user');
    if (m) return typeof m.content === 'string' ? m.content : m.content.map((c) => (c.type === 'text' ? c.text : '')).join('');
  }
  return undefined;
}

export type RailData = {
  sessionKey: string; title: string; view: View; meta: readonly Record<string, unknown>[];
  items: SealedArtifact[]; approvals: Approval[]; questions: Question[]; spend?: Spend;
  agent?: string; about?: { agent: string; created: string; activity: string };
};

/** The session's context: its pull request, versions, what waits on the person, cost and identity. */
export function RailBody(d: RailData) {
  return (
    <div className="flex flex-col gap-7">
      <RailSections sessionKey={d.sessionKey} title={d.title} opening={openingOf(d.view)} meta={d.meta} />
      <Versions items={d.items} />
      <NeedsYou approvals={d.approvals} questions={d.questions} />
      <Cost spend={d.spend} />
      <SessionInfo agent={d.about?.agent ?? d.agent} created={d.about?.created} activity={d.about?.activity} />
    </div>
  );
}

/** From 1440px the rail sits beside the ledger. */
export function Rail({ children }: { children: ReactNode }) {
  return <aside aria-label="Context" className="w-[296px] flex-none overflow-y-auto border-l border-line px-6 py-8">{children}</aside>;
}

/** Below 1440px the same content opens as a side sheet, or a bottom sheet on a phone. */
export function RailSheet({ onClose, children }: { onClose: () => void; children: ReactNode }) {
  return (
    <Dialog label="Details" onClose={onClose} className="fixed inset-x-0 bottom-0 z-50 flex max-h-[85dvh] flex-col rounded-t-[20px] border-t border-line bg-canvas shadow-lift sm:inset-y-0 sm:right-0 sm:left-auto sm:max-h-none sm:w-[min(360px,92vw)] sm:rounded-none sm:border-t-0 sm:border-l">
        <div className="flex items-center border-b border-line px-6 pt-2 pb-2">
          <h2 className="m-0 text-[15px] font-semibold">Details</h2>
          <button data-autofocus type="button" aria-label="Close" onClick={onClose} className="-mr-3 ml-auto grid h-11 w-11 cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 hover:bg-soft hover:text-ink">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
          </button>
        </div>
        <div className="flex-1 overflow-y-auto px-6 py-8 pb-[calc(2rem+var(--safe-b))]">{children}</div>
    </Dialog>
  );
}

/** Whether the rail has room (from 1440px); below that, Details opens it as a sheet. */
export function useDetails() {
  const wide = useMedia(WIDE);
  const [open, setOpen] = useState(false);
  return { wide, open: open && !wide, show: wide ? undefined : () => setOpen(true), close: () => setOpen(false) };
}

/** Beside the ledger when wide, else the sheet Details opens; each placement renders only when it applies. */
export function ContextRail({ details, placement, ...data }: RailData & { details: ReturnType<typeof useDetails>; placement: 'rail' | 'sheet' }) {
  if (placement === 'rail') return details.wide ? <Rail><RailBody {...data} /></Rail> : null;
  return details.open ? <RailSheet onClose={details.close}><RailBody {...data} /></RailSheet> : null;
}

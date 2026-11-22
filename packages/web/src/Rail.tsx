import { Fragment, useState, type ReactNode } from 'react';
import { listAgents, type AgentSummary, type Approval, type Question, type SealedArtifact, type Spend } from './api.ts';
import { ledgerId } from './Artifacts.tsx';
import { delivered, versionOf, type MarkOf } from './delivered.ts';
import { describe } from './Needs.tsx';
import { compact, needId, spendLabel } from './sessions.ts';
import { SpendChip } from './SpendChip.tsx';
import { Dialog } from './Dialog.tsx';
import { useLoad } from './useLoad.ts';
import type { View } from './view.ts';
import { useMedia, WIDE } from './useDesktop.ts';
import { RailSections } from './slots.tsx';
import { EXTENSIONS } from './extensions/index.ts';
import { verdictMark, VersionMark } from './VersionMenu.tsx';

const head = 'm-0 mb-3 text-xs font-semibold tracking-[.06em] text-muted uppercase';
const jumpBtn = 'flex min-h-11 w-full cursor-pointer items-start border-0 bg-transparent p-0 text-left text-[13px] text-ink lg:min-h-8';

// scrollIntoView ignores the CSS reduced-motion rule when asked for smooth, so the choice is made here.
function jump(id: string) {
  const calm = matchMedia('(prefers-reduced-motion: reduce)').matches;
  document.getElementById(id)?.scrollIntoView({ behavior: calm ? 'auto' : 'smooth', block: 'start' });
}

const clock = (at: string | number) => new Date(at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });

// Older versions stay one click away so a long revision history does not stretch the rail.
const RECENT_VERSIONS = 3;

// The row shows the number and a mark; what it means, where it went and when are its tooltip and spoken label.
function detail(a: SealedArtifact, markOf: MarkOf): string {
  const postedTo = EXTENSIONS.map((e) => e.artifact?.(a, false)).find(Boolean)?.postedTo;
  const posted = a.links?.length ? (postedTo ?? 'Posted') : '';
  return [`v${versionOf(a)}`, markOf(a).status, posted, clock(a.sealedAt)].filter(Boolean).join(', ');
}
const byVerdict: MarkOf = (a) => verdictMark(a.verdict);

function Versions({ items, markOf }: { items: SealedArtifact[]; markOf: MarkOf }) {
  const [all, setAll] = useState(false);
  const kind = items.reduce<SealedArtifact | undefined>((n, a) => (!n || a.sealedAt >= n.sealedAt ? a : n), undefined)?.kind;
  const every = delivered(items.filter((a) => a.kind === kind));
  if (!every.length) return null;
  const older = every.length - RECENT_VERSIONS;
  const rows = all ? every : every.slice(0, RECENT_VERSIONS);
  return (
    <section aria-labelledby="rail-versions">
      <h2 id="rail-versions" className={head}>Versions</h2>
      <ul id="rail-version-list" className="m-0 flex list-none flex-col gap-1 p-0">
        {rows.map((a) => (
          <li key={ledgerId(a)}>
            <button type="button" aria-label={detail(a, markOf)} title={detail(a, markOf)} onClick={() => jump(ledgerId(a))} className={`${jumpBtn.replace('items-start', 'items-center')} gap-2`}>
              <span className="font-mono font-semibold">{`v${versionOf(a)}`}</span><VersionMark mark={markOf(a).mark} />
            </button>
          </li>
        ))}
      </ul>
      {older > 0 && (
        <button type="button" aria-expanded={all} aria-controls="rail-version-list" onClick={() => setAll(!all)} className="mt-1 min-h-11 cursor-pointer border-0 bg-transparent px-0 text-left text-[13px] text-muted underline-offset-2 hover:text-ink hover:underline lg:min-h-8">
          {all ? 'Fewer' : 'Older…'}
        </button>
      )}
    </section>
  );
}

function NeedsYou({ approvals, questions }: { approvals: Approval[]; questions: Question[] }) {
  const lines = [
    ...questions.map((q) => ({ id: q.id, text: q.title ?? q.question })),
    ...approvals.map((a) => ({ id: a.id, text: describe(a.args) ?? a.tool })),
  ];
  if (lines.length === 0) return null;
  return (
    <section aria-labelledby="rail-needs">
      <h2 id="rail-needs" className={head}>Needs you</h2>
      <ul className="m-0 flex list-none flex-col gap-1 p-0">
        {lines.map((l) => <li key={l.id}><button type="button" onClick={() => jump(needId(l.id))} className={`${jumpBtn} text-warning`}>{l.text}</button></li>)}
      </ul>
    </section>
  );
}

function Cost({ spend }: { spend?: Spend }) {
  if (!spend || !spendLabel(spend)) return null;
  const { input, output, cacheRead } = spend.tokens;
  return (
    <section aria-labelledby="rail-cost">
      <h2 id="rail-cost" className={head}>Cost</h2>
      {spend.cost > 0 && (
        <p className="m-0 mb-3 flex items-baseline gap-2">
          <SpendChip spend={spend} className="text-[28px] leading-none font-semibold" />
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
  const [provider, id] = model?.includes('/') ? [model.slice(0, model.indexOf('/')), model.slice(model.indexOf('/') + 1)] : [undefined, model];
  const rows: Array<[string, ReactNode]> = [
    ...(agent ? [['Agent', agent] as [string, ReactNode]] : []),
    ...(id ? [['Model', <>{id}{provider && <span className="text-muted"> · {provider}</span>}</>] as [string, ReactNode]] : []),
    ...(created ? [['Started', clock(created)] as [string, ReactNode]] : []),
    ...(created && activity ? [['Last activity', clock(activity)] as [string, ReactNode]] : []),
  ];
  return (
    <section aria-labelledby="rail-session" className="border-t border-line pt-7">
      <h2 id="rail-session" className={head}>Session</h2>
      <dl className="m-0 grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-[13px]">
        {rows.map(([label, value]) => <Fragment key={label}><dt className="text-muted">{label}</dt><dd className="m-0 [overflow-wrap:anywhere]">{value}</dd></Fragment>)}
      </dl>
    </section>
  );
}

// The trigger text of a review is the first user message; the rail reads the pull request's branches from it.
export function openingOf(view: View): string | undefined {
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
  /** Each version's mark; its verdict when absent. */ markOf?: MarkOf;
};

/** The session's context: its pull request, versions, what waits on the person, cost and identity. */
export function RailBody(d: RailData) {
  return (
    <div className="flex flex-col gap-7">
      <RailSections sessionKey={d.sessionKey} title={d.title} opening={openingOf(d.view)} meta={d.meta} />
      <Versions items={d.items} markOf={d.markOf ?? byVerdict} />
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

/** Whether the rail has room (from 1440px, when nothing else takes the `column` beside the chat); otherwise Details opens it as a sheet. */
export function useDetails(column = true) {
  const wide = useMedia(WIDE) && column;
  const [open, setOpen] = useState(false);
  return { wide, open: open && !wide, show: wide ? undefined : () => setOpen(true), close: () => setOpen(false) };
}

/** Beside the ledger when wide, else the sheet Details opens; each placement renders only when it applies. */
export function ContextRail({ details, placement, ...data }: RailData & { details: ReturnType<typeof useDetails>; placement: 'rail' | 'sheet' }) {
  if (placement === 'rail') return details.wide ? <Rail><RailBody {...data} /></Rail> : null;
  return details.open ? <RailSheet onClose={details.close}><RailBody {...data} /></RailSheet> : null;
}

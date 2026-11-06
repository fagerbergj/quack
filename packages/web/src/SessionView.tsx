import { memo, useCallback, useEffect, useRef, useState, type CSSProperties, type ReactNode, type SubmitEvent } from 'react';
import { abortRun, archiveSession, decideApproval, deleteSession, exportUrl, getMeta, renameSession, sealedFrom, socketUrl, type Approval, type Failure, type Question, type SealedArtifact, type Spend } from './api.ts';
import { DeleteDialog, RenameDialog } from './Housekeeping.tsx';
import { AttachButton, Pending, useAttachments, type Img } from './Attach.tsx';
import { ApprovalCards, useApprovals } from './Approvals.tsx';
import { SessionQuestions } from './Questions.tsx';
import { SessionFailures } from './Failures.tsx';
import { artifactName } from './Artifacts.tsx';
import { CheckProgress, liveChecks, progressLabel } from './CheckResult.tsx';
import { SessionKeyContext, type StudioProps } from './extension.ts';
import { Work } from './Work.tsx';
import { ContextRail, useDetails } from './Rail.tsx';
import { useVisualViewportHeight } from './useVisualViewportHeight.ts';
import { useMenu } from './useMenu.ts';
import { useDesktop } from './useDesktop.ts';
import { stepLabel } from './step.ts';
import { externalLink, pendingInput } from './sessions.ts';
import { SpendChip } from './SpendChip.tsx';
import { SessionChips, studioFor } from './slots.tsx';
import type { Look, SessionTitle, Tone as LookTone } from './kinds.ts';
import { Transcript, Working } from './Transcript.tsx';
import { useLoad } from './useLoad.ts';
import { applyView, emptyView, type View, type ViewFrame } from './view.ts';

type Standing = { questions: Question[]; failures: Failure[]; approvals: Approval[] };
type Queued = { id: string; text: string };
type Frame = { type: 'queued'; items: Queued[] } | { type: 'artifacts'; artifacts: SealedArtifact[] } | ViewFrame | ({ type: 'standing' } & Standing) | { type: 'error'; error: string; requestId?: string } | { type: 'accepted'; requestId?: string };

let nextRequest = 0;

type SocketState = 'connecting' | 'open' | 'reconnecting';

function useSocket(key: string, agent: string | undefined) {
  const ws = useRef<WebSocket | null>(null);
  const held = useRef<View>(emptyView);
  const [view, setView] = useState<View>(emptyView);
  const [state, setState] = useState<SocketState>('connecting');
  const [error, setError] = useState('');
  const [standing, setStanding] = useState<Standing>({ questions: [], failures: [], approvals: [] });
  const [artifacts, setArtifacts] = useState<SealedArtifact[]>([]);
  const [queued, setQueued] = useState<Queued[]>([]);
  // One per input in flight, keyed by the requestId the server echoes on its `accepted` or `error`; it handles inputs concurrently, so answers can arrive out of order.
  const acks = useRef(new Map<string, (ok: boolean) => void>());
  const settle = (id: string | undefined, ok: boolean) => {
    acks.current.get(id ?? '')?.(ok);
    acks.current.delete(id ?? '');
  };
  useEffect(() => {
    let attempt = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let disposed = false;
    const connect = () => {
      held.current = emptyView;
      const sock = new WebSocket(socketUrl(key, agent));
      ws.current = sock;
      sock.onopen = () => {
        attempt = 0;
        setState('open');
      };
      // Every connect starts with a full view that replaces the one held, so a reconnect cannot duplicate lines.
      sock.onclose = () => {
        acks.current.forEach((done) => done(false));
        acks.current.clear();
        if (disposed) return;
        setState('reconnecting');
        timer = setTimeout(connect, Math.min(1000 * 2 ** attempt++, 30_000));
      };
      sock.onmessage = (ev) => {
        let frame: Frame;
        try {
          frame = JSON.parse(String(ev.data)) as Frame;
        } catch {
          return; // one malformed frame must not stop the ones after it
        }
        if (frame.type === 'view') {
          const next = applyView(held.current, frame);
          held.current = next;
          setView(next);
        } else if (frame.type === 'standing') setStanding(frame);
        else if (frame.type === 'artifacts') setArtifacts(sealedFrom(frame.artifacts));
        else if (frame.type === 'queued') setQueued(frame.items);
        else if (frame.type === 'error') (setError(frame.error), settle(frame.requestId, false));
        else if (frame.type === 'accepted') settle(frame.requestId, true);
      };
    };
    connect();
    return () => {
      disposed = true;
      clearTimeout(timer);
      ws.current?.close();
    };
  }, [key, agent]);
  // Resolves true once the server accepts the input, false when it rejects it or the socket drops, so a composer clears only on true.
  const send = useCallback((input: string, whenBusy?: 'steer' | 'followUp', images: Img[] = []): Promise<boolean> => {
    if (ws.current?.readyState !== WebSocket.OPEN) {
      setError('Disconnected: reconnecting. Your message was not sent.');
      return Promise.resolve(false);
    }
    setError('');
    // Not randomUUID: it is missing on a page served over plain http.
    const requestId = `${Date.now().toString(36)}-${(nextRequest += 1)}`;
    const done = new Promise<boolean>((resolve) => acks.current.set(requestId, resolve));
    ws.current.send(JSON.stringify({ type: 'input', requestId, input, images: images.length ? images : undefined, whenBusy }));
    return done;
  }, []);
  // withdraw drops a queued follow-up on the server; steer takes it back and sends it into the running work.
  const unqueue = useCallback((type: 'withdraw' | 'steer', id: string) => void (ws.current?.readyState === WebSocket.OPEN && ws.current.send(JSON.stringify({ type, id }))), []);
  // An earlier or gap row asks the server for the entries the window does not hold.
  const load = useCallback((req: { type: 'earlier' } | { type: 'gap'; after: number; upTo: number }) => void (ws.current?.readyState === WebSocket.OPEN && ws.current.send(JSON.stringify(req))), []);
  return { view, state, error, setError, standing, artifacts, queued, send, unqueue, load };
}

const chipBtn = 'h-11 flex-none cursor-pointer rounded-[10px] border-0 bg-transparent text-[13px] lg:h-8';

// A message sent while a run works is a follow-up queued in the session, so a reload or a closed tab keeps it.
function QueuedChip({ text, onNow, onRemove }: { text: string; onNow: () => void; onRemove: () => void }) {
  return (
    <div className="pointer-events-auto flex max-w-[92%] items-center gap-1 self-end rounded-2xl border border-line-strong bg-soft py-0.5 pr-0.5 pl-3">
      <span className="flex-none text-[12px] text-muted">Queued</span>
      <span className="min-w-0 truncate text-[13.5px]">{text}</span>
      <button type="button" onClick={onNow} className={`${chipBtn} px-2.5 text-link`}>Send now</button>
      <button type="button" aria-label="Remove queued message" onClick={onRemove} className={`${chipBtn} grid w-11 place-items-center text-muted hover:text-ink lg:w-8`}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
      </button>
    </div>
  );
}

type Unqueue = (type: 'withdraw' | 'steer', id: string) => void;

type Send = (i: string, w?: 'steer' | 'followUp', images?: Img[]) => Promise<boolean>;

// The composer owns its draft, so a keystroke re-renders it alone, never the transcript.
const Composer = memo(function Composer({ sessionKey, state, busy, send, queued, unqueue }: { sessionKey: string; state: SocketState; busy: boolean; send: Send; queued: Queued[]; unqueue: Unqueue }) {
  const { input, setInput, attach } = useSeed(sessionKey, state, send);
  const [sending, setSending] = useState(false);
  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    if (sending || (!input.trim() && !attach.images.length)) return;
    setSending(true);
    // A rejected send keeps the text and images so the person can fix it and send again.
    if (await send(input, busy ? 'followUp' : undefined, attach.images)) (setInput(''), attach.clear());
    setSending(false);
  };
  const empty = !input.trim() && !attach.images.length;
  return (
    <>
      {queued.map((q) => <QueuedChip key={q.id} text={q.text} onNow={() => unqueue('steer', q.id)} onRemove={() => unqueue('withdraw', q.id)} />)}
      <Pending attach={attach} />
      <form onSubmit={submit} {...attach.handlers} className="pointer-events-auto flex items-center gap-1 rounded-[26px] bg-soft py-1.5 pr-1.5 pl-1 shadow-lift ring-1 ring-line-strong transition focus-within:ring-ink-2/40">
        <AttachButton attach={attach} />
        <label htmlFor="steer" className="sr-only">Message</label>
        <textarea id="steer" value={input} onChange={(e) => setInput(e.target.value)} rows={1} placeholder={busy ? 'Message · sends after this step' : 'Message'} className="max-h-[220px] min-h-[40px] min-w-0 flex-1 resize-none bg-transparent px-1 py-2 text-[15px] leading-relaxed outline-none placeholder:text-muted" />
        <button type="submit" disabled={state !== 'open' || empty || sending} aria-label="Send" className="grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-ink text-on-ink hover:bg-ink-2 disabled:cursor-not-allowed disabled:opacity-50">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M12 19V5M6 11l6-6 6 6" /></svg>
        </button>
      </form>
    </>
  );
});

type Tone = 'wait' | 'run' | 'idle' | 'fail';
const TONES: Record<Tone, string> = {
  wait: 'text-warning lg:bg-warning-bg',
  run: 'text-info lg:bg-info-bg',
  idle: 'text-muted lg:bg-soft',
  fail: 'text-danger lg:bg-danger-bg',
};

// A line under the title on phones, a pill beside it from lg.
function StatusChip({ text, tone }: { text: string; tone: Tone }) {
  return (
    <span role="status" className={`flex items-center gap-1.5 text-[12px] lg:rounded-full lg:px-2 lg:py-0.5 lg:font-semibold ${TONES[tone]}`}>
      {tone === 'run' && <span aria-hidden="true" className="live-dot h-1.5 w-1.5 rounded-full bg-info" />}
      {text}
    </span>
  );
}

const ICON = { width: 18, height: 18, viewBox: '0 0 24 24', 'aria-hidden': true } as const;
const iconBtn = 'grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 no-underline hover:text-ink lg:h-9 lg:w-9';

// The name comes from sessionTitle, the one place a session is named.
function Title({ name }: { name: SessionTitle }) {
  const lead = [name.repo, name.number].filter(Boolean).join(' ');
  return (
    <h1 className="m-0 min-w-0 truncate font-sans text-[15px] font-semibold" title={name.full}>
      {lead && <span className="font-medium text-muted">{`${lead} `}</span>}
      {name.text}
    </h1>
  );
}

type Manage = { archived: boolean; changed: () => void };

type Entry = { label: string; run?: () => void; href?: string; danger?: boolean };

function MenuEntry({ e, close }: { e: Entry; close: () => void }) {
  const cls = `flex min-h-11 w-full cursor-pointer items-center border-0 bg-transparent px-3 text-left text-[13px] no-underline hover:bg-soft lg:min-h-8 ${e.danger ? 'text-danger' : 'text-ink'}`;
  return e.href
    ? <a role="menuitem" tabIndex={-1} href={e.href} download onClick={() => close()} className={cls}>{e.label}</a>
    : <button type="button" role="menuitem" tabIndex={-1} onClick={() => { close(); e.run?.(); }} className={cls}>{e.label}</button>;
}

type MenuActions = { archived: boolean; exportHref: string; rename: () => void; archive: () => void; remove: () => void };

// Grouped from harmless to destructive: stop the run, everyday actions, reversible archive, then delete last.
function groupsOf(running: boolean, latest: SealedArtifact | undefined, onAbort: () => void, onOpen: (a: SealedArtifact) => void, manage?: MenuActions): Entry[][] {
  return [
    running ? [{ label: 'Stop', run: onAbort }] : [],
    [
      ...(latest ? [{ label: `Open ${artifactName(latest).toLowerCase()}`, run: () => onOpen(latest) }] : []),
      ...(manage ? [{ label: 'Rename', run: manage.rename }, { label: 'Export', href: manage.exportHref }] : []),
    ],
    manage ? [manage.archived ? { label: 'Unarchive', run: manage.archive } : { label: 'Archive', run: manage.archive }] : [],
    manage ? [{ label: 'Delete', run: manage.remove, danger: true }] : [],
  ].filter((g) => g.length);
}

// Stopping the run, opening the latest output and housekeeping live behind one quiet menu; it is absent when none applies.
function More({ running, onAbort, latest, onOpen, manage }: { running: boolean; onAbort: () => void; latest?: SealedArtifact; onOpen: (a: SealedArtifact) => void; manage?: MenuActions }) {
  const menu = useMenu();
  if (!running && !latest && !manage) return null;
  return (
    <div ref={menu.root} onKeyDown={menu.keys} className="relative">
      <button ref={menu.button} type="button" aria-label="More actions" aria-haspopup="menu" aria-expanded={menu.open} onClick={menu.toggle} className={iconBtn}><svg {...ICON} fill="currentColor"><circle cx="5" cy="12" r="1.8" /><circle cx="12" cy="12" r="1.8" /><circle cx="19" cy="12" r="1.8" /></svg></button>
      {menu.open && (
        <div role="menu" aria-label="More actions" className="absolute right-0 z-20 mt-1 min-w-44 rounded-xl border border-line-strong bg-surface py-1 shadow-float">
          {groupsOf(running, latest, onAbort, onOpen, manage).map((g, i) => (
            <div key={i} role="group" className={i ? 'mt-1 border-t border-line pt-1' : ''}>{g.map((e) => <MenuEntry key={e.label} e={e} close={menu.close} />)}</div>
          ))}
        </div>
      )}
    </div>
  );
}

const RESULT: Record<LookTone, string> = { plain: 'text-ink-2', muted: 'text-muted', warn: 'text-warning', ok: 'text-success', bad: 'text-danger' };

// What the session delivered (a review's posted verdict and its link); absent while it runs, so an earlier result never reads as current.
function Result({ result }: { result: Look }) {
  return (
    <span className={`flex items-center gap-2 text-[12px] ${RESULT[result.tone]}`}>
      {result.line}
      {result.link && <a href={result.link.url} target="_blank" rel="noopener noreferrer" className="-my-3.5 py-3.5 text-link no-underline hover:underline">{`${result.link.label} ↗`}</a>}
    </span>
  );
}

function Header({ sessionKey, name, back, status, tone, spend, link, result, onDetails, children }: { sessionKey: string; name: SessionTitle; back: { href: string; label: string }; status: string; tone: Tone; spend?: Spend; link?: { url: string; label: string }; result?: Look; onDetails?: () => void; children: ReactNode }) {
  return (
    <div className="flex items-center gap-1 border-b border-line bg-canvas/70 px-1.5 pt-[calc(0.375rem+env(safe-area-inset-top,0px))] pb-1.5 lg:h-14 lg:gap-3 lg:px-5 lg:py-0">
        <a href={back.href} aria-label={back.label} className="grid h-11 w-11 flex-none place-items-center text-ink-2 no-underline hover:text-ink lg:hidden">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M15 5l-7 7 7 7" /></svg>
        </a>
        <span aria-hidden="true" className="hidden h-[26px] w-[26px] flex-none place-items-center rounded-lg bg-link-bg text-link lg:grid">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d={name.icon} /></svg>
        </span>
        <div className="flex min-w-0 flex-1 flex-col lg:flex-row lg:items-center lg:gap-3">
          <Title name={name} />
          <StatusChip text={status} tone={tone} />
          {result && <Result result={result} />}
        </div>
        <SessionChips sessionKey={sessionKey} />
        <SpendChip spend={spend} className="mr-1 flex-none text-[12.5px] text-muted" />
        {onDetails && <button type="button" onClick={onDetails} className="h-11 flex-none cursor-pointer rounded-full border-0 bg-transparent px-3 text-[13px] text-link hover:bg-soft lg:h-9">Details</button>}
        {link && (
          <a href={link.url} target="_blank" rel="noopener noreferrer" aria-label={link.label} title={link.label} className={iconBtn}>
            <svg {...ICON} fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"><path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" /></svg>
          </a>
        )}
        {children}
      </div>
  );
}

function statusOf(state: SocketState, waiting: number, running: boolean, failed?: Failure): [string, Tone] {
  if (state !== 'open') return [state === 'reconnecting' ? 'Reconnecting' : 'Connecting', 'idle'];
  if (waiting) return ['Waiting on you', 'wait'];
  if (failed && !running) return [`Failed · ${failed.step}`, 'fail'];
  return running ? ['Running', 'run'] : ['Idle', 'idle'];
}

// Below lg the pane is the visible viewport above the 48px tab bar (Shell) and draws the only top bar, so the composer stays above the keyboard.
function usePaneStyle(): CSSProperties | undefined {
  const vv = useVisualViewportHeight();
  if (vv === undefined) return undefined;
  const chrome = vv < window.innerHeight - 150 ? '0px' : '48px - var(--safe-b)';
  return { '--ws-h': `calc(${vv}px - ${chrome})` } as CSSProperties;
}

// The landing composer's text and images seed this composer; they are sent once when the socket opens (queued behind a run if the session, like Home's standing chat, is busy), and only while
// the composer still holds the untouched seed, so an edit or a failed connect leaves them to the user.
function useSeed(sessionKey: string, state: SocketState, send: Send) {
  const [seed] = useState(() => pendingInput.get(sessionKey));
  const [input, setInput] = useState(seed?.text ?? '');
  const attach = useAttachments(seed?.images);
  useEffect(() => void pendingInput.delete(sessionKey), [sessionKey]);
  useEffect(() => {
    if (state === 'open' && seed && input === seed.text && attach.images === seed.images) void send(input, 'followUp', seed.images).then((ok) => ok && (setInput(''), attach.clear()));
  }, [state]);
  return { input, setInput, attach };
}

// While the judge runs, its criteria and each result as it lands replace the bare step label.
function Running({ view, waiting, active }: { view: View; waiting: boolean; active: boolean }) {
  if (waiting) return <Working label="Waiting for you" />;
  if (!active) return null;
  const checks = liveChecks(view.tool?.details);
  if (!checks) return <Working label={stepLabel(view)} />;
  return (
    <>
      <Working label={progressLabel(checks)} />
      <CheckProgress checks={checks} />
    </>
  );
}

const NO_META: Array<Record<string, unknown>> = [];

// The reason a studio's approval is declined with when the person writes instead of approving; their message says the rest.
export const SUPERSEDED = 'Sent a message instead';

// A studio session shows its pane beside the transcript, and decides the approvals the pane owns there rather than as transcript cards.
function useStudio(sessionKey: string, approvals: Approval[], send: Send, decide: (id: string) => void, setError: (e: string) => void, running: boolean) {
  const studio = studioFor(sessionKey);
  const [tab, setTab] = useState<'pane' | 'chat'>('pane');
  if (!studio) return { studio, tab, setTab, send, cards: approvals, busy: running, chat: '' };
  const owned = approvals.filter(studio.owns);
  // Steered, the message reaches the run the moment the declined approval returns, instead of queueing behind a wait.
  const steer: Send = async (input, whenBusy, images) => {
    if (!owned.length) return send(input, whenBusy, images);
    if (!(await send(input, 'steer', images))) return false;
    await Promise.all(owned.map((a) => decideApproval(sessionKey, a.id, 'reject', SUPERSEDED).then(() => decide(a.id), (e: Error) => setError(e.message))));
    return true;
  };
  // Below lg the chat column shows only on its tab; from lg it is a fixed column beside the pane.
  const chat = `lg:max-w-[440px] lg:border-r lg:border-line ${tab === 'chat' ? '' : 'max-lg:hidden'}`;
  return { studio, tab, setTab, send: steer, cards: approvals.filter((a) => !studio.owns(a)), busy: running && !owned.length, chat };
}
type Live = ReturnType<typeof useStudio>;

// Below lg a studio session is one pane at a time; the chat tab counts the questions waiting there.
function StudioTabs({ live: { studio, tab, setTab }, waiting }: { live: Live; waiting: number }) {
  if (!studio) return null;
  const cls = 'flex min-h-11 flex-1 cursor-pointer items-center justify-center gap-1.5 border-0 border-b-2 border-transparent bg-transparent text-[13.5px] text-muted aria-selected:border-ink aria-selected:font-semibold aria-selected:text-ink';
  return (
    <div role="tablist" aria-label="Show" className="flex border-b border-line lg:hidden">
      <button type="button" role="tab" aria-selected={tab === 'pane'} onClick={() => setTab('pane')} className={cls}>{studio.label}</button>
      <button type="button" role="tab" aria-selected={tab === 'chat'} onClick={() => setTab('chat')} className={cls}>
        Chat{waiting > 0 && ' '}{waiting > 0 && <span className="rounded-full bg-warning-bg px-1.5 text-[11px] font-semibold text-warning">{waiting}</span>}
      </button>
    </div>
  );
}

// Beside the chat: the studio's pane when the session has one, else the context rail.
function Side({ live, rail, ...props }: Omit<StudioProps, 'visible'> & { live: Live; rail: ReactNode }) {
  const Pane = live.studio?.Pane;
  // From lg the pane sits beside the chat; below it, only its tab shows it.
  const visible = useDesktop() || live.tab === 'pane';
  if (!Pane) return rail;
  return (
    <section aria-label={live.studio!.label} className={`min-h-0 min-w-0 flex-1 overflow-y-auto ${live.tab === 'pane' ? '' : 'max-lg:hidden'}`}>
      <Pane {...props} visible={visible} />
    </section>
  );
}

export function SessionView({ sessionKey, agent, name, delivered, spend, manage, about }: { sessionKey: string; agent?: string; name: SessionTitle; delivered?: Look; spend?: Spend; manage?: Manage; about?: { agent: string; created: string; activity: string } }) {
  const { view, state, error, setError, standing, artifacts: items, queued, send, unqueue, load } = useSocket(sessionKey, agent);
  const [dialog, setDialog] = useState<'rename' | 'delete' | null>(null);
  const abort = () => abortRun(sessionKey).catch((e: Error) => setError(e.message));
  const [work, setWork] = useState<SealedArtifact | null>(null);
  const pending = useApprovals(standing.approvals);
  const approvals = pending.items;
  const waiting = approvals.length + standing.questions.length;
  const details = useDetails();
  const open = state === 'open';
  const [status, tone] = statusOf(state, waiting, view.running, standing.failures[0]);
  const paneStyle = usePaneStyle();
  // A new invocation records its meta before its run starts, so a run starting is when to look again.
  const { data: meta } = useLoad(() => getMeta(sessionKey), [sessionKey, view.running], NO_META);
  const rail = { sessionKey, title: name.text, view, meta, items, approvals, questions: standing.questions, spend: spend ?? view.spend, agent, about };
  const live = useStudio(sessionKey, approvals, send, pending.decide, setError, view.running);
  return (
    <div style={paneStyle} className="group/pane flex h-(--ws-h) min-h-0 flex-col lg:h-auto lg:flex-1">
      <Header sessionKey={sessionKey} name={name} back={sessionKey.startsWith('home:') ? { href: '#/', label: 'Home' } : { href: '#/sessions', label: 'Sessions' }} status={status} tone={tone} spend={spend ?? view.spend} link={externalLink(sessionKey)} result={tone === 'run' ? undefined : delivered} onDetails={details.show}>
        {manage?.archived && <span className="rounded-full bg-soft px-2 py-0.5 text-[12px] text-muted">Archived</span>}
        <More running={state === 'open' && view.running} onAbort={abort} latest={items.at(-1)} onOpen={setWork} manage={manage && { archived: manage.archived, exportHref: exportUrl(sessionKey), rename: () => setDialog('rename'), remove: () => setDialog('delete'), archive: () => archiveSession(sessionKey, !manage.archived).then(manage.changed, (e: Error) => setError(e.message)) }} />
      </Header>
      {manage && dialog === 'rename' && <RenameDialog current={name.full} save={(t) => renameSession(sessionKey, t).then(manage.changed)} onClose={() => setDialog(null)} />}
      {manage && dialog === 'delete' && <DeleteDialog name={name.full} remove={() => deleteSession(sessionKey)} onClose={() => setDialog(null)} onDone={() => { location.hash = '#/'; manage.changed(); }} />}
      <StudioTabs live={live} waiting={standing.questions.length} />
      <div className="flex min-h-0 flex-1">
      <div className={`relative flex min-h-0 min-w-0 flex-1 flex-col ${live.chat}`}>
        <div className="relative mx-auto flex min-h-0 w-full max-w-[860px] min-w-0 flex-1 flex-col overflow-y-auto px-4 pt-5 pb-36 sm:px-5">
          <div className="flex min-w-0 flex-col gap-5 [overflow-wrap:anywhere] [&_pre]:max-w-full [&_pre]:overflow-x-auto">
            {error && <p role="alert" className="m-0 rounded-xl border border-danger-line bg-danger-bg/60 px-3.5 py-2.5 text-[13px] text-danger">{error}</p>}
            <SessionKeyContext value={sessionKey}>
              <Transcript view={view} meta={meta} load={load} artifacts={items} running={tone === 'run'} onOpen={setWork} />
            </SessionKeyContext>
            <ApprovalCards sessionKey={sessionKey} title={name.text} items={live.cards} decide={pending.decide} />
            <SessionQuestions sessionKey={sessionKey} items={standing.questions} />
            <SessionFailures sessionKey={sessionKey} items={standing.failures} />
            <Running view={view} waiting={waiting > 0} active={tone === 'run'} />
          </div>
        </div>
        {/* On a phone the keyboard leaves room for an open answer field or the composer, not both; focus would flicker it under a tap. */}
        <div className="pointer-events-none absolute inset-x-0 bottom-0 max-lg:group-has-[[data-answer]]/pane:hidden bg-linear-to-b from-canvas/0 via-canvas/90 via-40% to-canvas px-3 pt-10 pb-[max(0.75rem,var(--safe-b))] sm:px-4">
          <div className="mx-auto flex max-w-[860px] flex-col gap-2">
            <Composer sessionKey={sessionKey} state={state} busy={live.busy} send={live.send} queued={queued} unqueue={unqueue} />
          </div>
        </div>
      </div>
      <Side live={live} sessionKey={sessionKey} artifacts={items} approvals={approvals} running={view.running} decided={pending.decide} rail={<ContextRail {...rail} details={details} placement="rail" />} />
      </div>
      <ContextRail {...rail} details={details} placement="sheet" />
      {work && <Work artifact={work} session={sessionKey} onClose={() => setWork(null)} />}
    </div>
  );
}

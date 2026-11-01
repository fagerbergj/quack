import type { AgentEvent, SnapshotEvent } from '@earendil-works/pi-durable';
import { useEffect, useRef, useState, type CSSProperties, type ReactNode, type SubmitEvent } from 'react';
import { abortRun, socketUrl, type SealedArtifact } from './api.ts';
import { Approvals } from './Approvals.tsx';
import { SessionDecisions } from './Decisions.tsx';
import { SessionFailures } from './Failures.tsx';
import { Artifacts, useArtifacts } from './Artifacts.tsx';
import { Work } from './Work.tsx';
import { useVisualViewportHeight } from './useVisualViewportHeight.ts';
import { githubUrl, pendingInput } from './sessions.ts';
import type { SessionTitle } from './kinds.ts';
import { Transcript, Working } from './Transcript.tsx';
import { applyEvents, emptyView, snapshotView, touchesApprovals, type View } from './view.ts';

type Frame = SnapshotEvent | { type: 'events'; events: AgentEvent[] } | { type: 'error'; error: string } | { type: 'accepted' };

function useSocket(key: string, agent: string | undefined) {
  const ws = useRef<WebSocket | null>(null);
  const [view, setView] = useState<View>(emptyView);
  const [state, setState] = useState<'connecting' | 'open' | 'reconnecting'>('connecting');
  const [error, setError] = useState('');
  const [approvalTick, setTick] = useState(0);
  useEffect(() => {
    let attempt = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let disposed = false;
    const connect = () => {
      const sock = new WebSocket(socketUrl(key, agent));
      ws.current = sock;
      sock.onopen = () => {
        attempt = 0;
        setState('open');
      };
      // Every connect starts with a full snapshot that replaces the view, so a reconnect cannot duplicate lines.
      sock.onclose = () => {
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
        if (frame.type === 'snapshot') setView(snapshotView(frame));
        else if (frame.type === 'events') {
          setView((v) => applyEvents(v, frame.events));
          if (touchesApprovals(frame.events)) setTick((t) => t + 1);
        } else if (frame.type === 'error') setError(frame.error);
      };
    };
    connect();
    return () => {
      disposed = true;
      clearTimeout(timer);
      ws.current?.close();
    };
  }, [key, agent]);
  const send = (input: string, whenBusy?: 'steer' | 'followUp') => {
    if (ws.current?.readyState !== WebSocket.OPEN) {
      setError('Disconnected: reconnecting. Your message was not sent.');
      return false;
    }
    setError('');
    ws.current.send(JSON.stringify({ type: 'input', input, whenBusy }));
    return true;
  };
  return { view, state, error, setError, approvalTick, send };
}

const seg = (on: boolean) =>
  `h-11 cursor-pointer rounded-[14px] border-0 px-2.5 text-[12.5px] max-lg:flex-1 lg:h-[30px] ${on ? 'bg-surface font-semibold text-ink shadow-thumb' : 'bg-transparent text-muted hover:text-ink'}`;

function Composer({ busy, disabled, send, input, setInput }: { busy: boolean; disabled: boolean; send: (i: string, w?: 'steer' | 'followUp') => boolean; input: string; setInput: (s: string) => void }) {
  const [mode, setMode] = useState<'steer' | 'followUp'>('steer');
  const submit = (e: SubmitEvent) => {
    e.preventDefault();
    if (!input.trim()) return;
    if (send(input, busy ? mode : undefined)) setInput('');
  };
  return (
    <form onSubmit={submit} className="flex flex-wrap items-center gap-x-2 rounded-[22px] border border-line-strong bg-surface py-1.5 pr-1.5 pl-3.5 shadow-card transition focus-within:border-ink-2/40 focus-within:shadow-ring lg:pl-2">
      {busy && (
        <div role="group" aria-label="While the agent is working" className="mt-1 inline-flex rounded-2xl bg-soft p-0.5 max-lg:basis-full lg:mt-0">
          <button type="button" aria-pressed={mode === 'steer'} onClick={() => setMode('steer')} className={seg(mode === 'steer')}>Steer now</button>
          <button type="button" aria-pressed={mode === 'followUp'} onClick={() => setMode('followUp')} className={seg(mode === 'followUp')}>Queue next</button>
        </div>
      )}
      <label htmlFor="steer" className="sr-only">Message</label>
      <textarea id="steer" value={input} onChange={(e) => setInput(e.target.value)} rows={1} placeholder={busy ? 'Steer this run…' : 'Message'} className="max-h-[220px] min-h-[40px] min-w-0 flex-1 resize-none bg-transparent px-1 py-2 text-[15px] leading-relaxed outline-none placeholder:text-muted" />
      <button type="submit" disabled={disabled} aria-label="Send" className="grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-ink text-on-ink hover:bg-ink-2 disabled:cursor-not-allowed disabled:opacity-50">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M12 19V5M6 11l6-6 6 6" /></svg>
      </button>
    </form>
  );
}

type Tone = 'wait' | 'run' | 'idle';
const TONES: Record<Tone, string> = {
  wait: 'text-warning lg:bg-warning-bg',
  run: 'text-info lg:bg-info-bg',
  idle: 'text-muted lg:bg-soft',
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

const compact = (n: number) => (n >= 1000 ? `${Math.round(n / 1000)}k` : String(n));

// Abort and the Work drawer live behind one quiet menu; it is absent when neither applies.
function More({ running, onAbort, latest, onOpen }: { running: boolean; onAbort: () => void; latest?: SealedArtifact; onOpen: (a: SealedArtifact) => void }) {
  const [open, setOpen] = useState(false);
  if (!running && !latest) return null;
  const item = 'flex min-h-11 w-full cursor-pointer items-center border-0 bg-transparent px-3 text-left text-[13px] hover:bg-soft lg:min-h-8';
  const pick = (fn: () => void) => () => { setOpen(false); fn(); };
  return (
    <div className="relative">
      <button type="button" aria-label="More actions" aria-expanded={open} onClick={() => setOpen(!open)} onKeyDown={(e) => e.key === 'Escape' && setOpen(false)} className={iconBtn}><svg {...ICON} fill="currentColor"><circle cx="5" cy="12" r="1.8" /><circle cx="12" cy="12" r="1.8" /><circle cx="19" cy="12" r="1.8" /></svg></button>
      {open && (
        <div role="menu" className="absolute right-0 z-20 mt-1 min-w-40 rounded-xl border border-line-strong bg-surface py-1 shadow-float">
          {latest && <button type="button" role="menuitem" onClick={pick(() => onOpen(latest))} className={`${item} text-ink`}>Work</button>}
          {running && <button type="button" role="menuitem" onClick={pick(onAbort)} className={`${item} text-danger`}>Abort</button>}
        </div>
      )}
    </div>
  );
}

function Header({ name, status, tone, tokens, github, children }: { name: SessionTitle; status: string; tone: Tone; tokens?: number; github?: string; children: ReactNode }) {
  return (
    <div className="flex items-center gap-1 border-b border-line bg-canvas/70 px-1.5 pt-[calc(0.375rem+env(safe-area-inset-top,0px))] pb-1.5 lg:h-14 lg:gap-3 lg:px-5 lg:py-0">
        <a href="#/" aria-label="Sessions" className="grid h-11 w-11 flex-none place-items-center text-ink-2 no-underline hover:text-ink lg:hidden">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M15 5l-7 7 7 7" /></svg>
        </a>
        <span aria-hidden="true" className="hidden h-[26px] w-[26px] flex-none place-items-center rounded-lg bg-link-bg text-link lg:grid">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d={name.icon} /></svg>
        </span>
        <div className="flex min-w-0 flex-1 flex-col lg:flex-row lg:items-center lg:gap-3">
          <Title name={name} />
          <StatusChip text={status} tone={tone} />
        </div>
        {tokens ? <span className="mr-1 hidden text-[12.5px] text-muted lg:inline">{`${compact(tokens)} tokens`}</span> : null}
        {github && (
          <a href={github} target="_blank" rel="noopener noreferrer" aria-label="View on GitHub" title="View on GitHub" className={iconBtn}>
            <svg {...ICON} fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round"><path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" /></svg>
          </a>
        )}
        {children}
      </div>
  );
}

function statusOf(state: 'connecting' | 'open' | 'reconnecting', waiting: number, running: boolean): [string, Tone] {
  if (state !== 'open') return [state === 'reconnecting' ? 'Reconnecting' : 'Connecting', 'idle'];
  if (waiting) return ['Waiting on you', 'wait'];
  return running ? ['Running', 'run'] : ['Idle', 'idle'];
}

// Below lg the pane is the visible viewport above the tab bar and draws the only top bar, so the composer stays above the keyboard.
function usePaneStyle(): CSSProperties | undefined {
  const vv = useVisualViewportHeight();
  if (vv === undefined) return undefined;
  const chrome = vv < window.innerHeight - 150 ? '0px' : '61px - env(safe-area-inset-bottom, 0px)';
  return { '--ws-h': `calc(${vv}px - ${chrome})` } as CSSProperties;
}

export function SessionView({ sessionKey, agent, name }: { sessionKey: string; agent?: string; name: SessionTitle }) {
  const { view, state, error, setError, approvalTick, send } = useSocket(sessionKey, agent);
  // The landing composer's text seeds this composer.
  const [initial] = useState(() => pendingInput.get(sessionKey) ?? '');
  const [input, setInput] = useState(initial);
  useEffect(() => void pendingInput.delete(sessionKey), [sessionKey]);
  // Sent once when the socket opens, and only while the composer still holds the untouched seed; an edit or a failed connect leaves it to the user.
  useEffect(() => {
    if (state === 'open' && input !== '' && input === initial && send(input)) setInput('');
  }, [state]);
  const abort = () => abortRun(sessionKey).catch((e: Error) => setError(e.message));
  const { items, error: artifactsError } = useArtifacts(sessionKey, approvalTick);
  const [work, setWork] = useState<SealedArtifact | null>(null);
  const [approvals, setApprovals] = useState(0);
  const [questions, setQuestions] = useState(0);
  const waiting = approvals + questions;
  const open = state === 'open';
  const [status, tone] = statusOf(state, waiting, view.running);
  const paneStyle = usePaneStyle();
  return (
    <div style={paneStyle} className="flex h-(--ws-h) min-h-0 flex-col lg:h-auto lg:flex-1">
      <Header name={name} status={status} tone={tone} tokens={view.tokens} github={githubUrl(sessionKey)}>
        <More running={state === 'open' && view.running} onAbort={abort} latest={items.at(-1)} onOpen={setWork} />
      </Header>
      <div className="mx-auto flex min-h-0 w-full max-w-[720px] min-w-0 flex-1 flex-col overflow-y-auto px-4 py-5 sm:px-5" aria-live="polite">
        <div className="flex min-w-0 flex-col gap-5 [overflow-wrap:anywhere] [&_pre]:max-w-full [&_pre]:overflow-x-auto">
          {error && <p role="alert" className="m-0 rounded-xl border border-danger-line bg-danger-bg/60 px-3.5 py-2.5 text-[13px] text-danger">{error}</p>}
          <Transcript view={view} />
          <Artifacts items={items} error={artifactsError} onOpen={setWork} />
          <Approvals sessionKey={sessionKey} refresh={approvalTick} onCount={setApprovals} />
          <SessionDecisions sessionKey={sessionKey} onCount={setQuestions} />
          <SessionFailures sessionKey={sessionKey} />
          {waiting > 0 ? <Working label="Waiting for you" /> : tone === 'run' && <Working label="Working" />}
        </div>
      </div>
      <div className="border-t border-line bg-canvas/90 px-3 pt-2 pb-[max(1.25rem,env(safe-area-inset-bottom,0px))] backdrop-blur-md sm:px-4 lg:border-t-0 lg:pt-2.5 lg:pb-5">
        <div className="mx-auto max-w-[720px]">
          <Composer busy={view.running} disabled={state !== 'open'} send={send} input={input} setInput={setInput} />
        </div>
      </div>
      {work && <Work artifact={work} onClose={() => setWork(null)} />}
    </div>
  );
}

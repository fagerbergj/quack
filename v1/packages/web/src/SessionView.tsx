import type { AgentEvent, SnapshotEvent } from '@earendil-works/pi-durable';
import { useEffect, useRef, useState, type CSSProperties, type SubmitEvent } from 'react';
import { abortRun, socketUrl } from './api.ts';
import { Approvals } from './Approvals.tsx';
import { Artifacts } from './Artifacts.tsx';
import { useVisualViewportHeight } from './useVisualViewportHeight.ts';
import { pendingInput } from './sessions.ts';
import { Transcript } from './Transcript.tsx';
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
        const first = pendingInput.get(key);
        if (first !== undefined) {
          pendingInput.delete(key);
          sock.send(JSON.stringify({ type: 'input', input: first }));
        }
      };
      // Every connect starts with a full snapshot that replaces the view, so a reconnect cannot duplicate lines.
      sock.onclose = () => {
        if (disposed) return;
        setState('reconnecting');
        timer = setTimeout(connect, Math.min(1000 * 2 ** attempt++, 30_000));
      };
      sock.onmessage = (ev) => {
        const frame = JSON.parse(String(ev.data)) as Frame;
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
  `h-11 lg:h-[26px] cursor-pointer rounded-full border-0 px-2.5 text-xs font-semibold ${on ? 'bg-surface text-ink shadow-thumb' : 'bg-transparent text-muted hover:text-ink'}`;

function Composer({ busy, disabled, send }: { busy: boolean; disabled: boolean; send: (i: string, w?: 'steer' | 'followUp') => boolean }) {
  const [input, setInput] = useState('');
  const [mode, setMode] = useState<'steer' | 'followUp'>('steer');
  const submit = (e: SubmitEvent) => {
    e.preventDefault();
    if (!input.trim()) return;
    if (send(input, busy ? mode : undefined)) setInput('');
  };
  return (
    <form onSubmit={submit} className="rounded-[22px] border border-line-strong bg-surface shadow-float transition focus-within:border-ink-2/40 focus-within:shadow-ring">
      {busy && (
        <div className="flex items-center gap-2 border-b border-line px-3 py-1.5">
          <div role="group" aria-label="While the agent is working" className="inline-flex rounded-full border border-line bg-soft p-[3px]">
            <button type="button" aria-pressed={mode === 'steer'} onClick={() => setMode('steer')} className={seg(mode === 'steer')}>Steer now</button>
            <button type="button" aria-pressed={mode === 'followUp'} onClick={() => setMode('followUp')} className={seg(mode === 'followUp')}>Queue next</button>
          </div>
        </div>
      )}
      <div className="flex items-end gap-1.5 p-2.5">
        <label htmlFor="steer" className="sr-only">Message</label>
        <textarea id="steer" value={input} onChange={(e) => setInput(e.target.value)} rows={1} placeholder={busy ? 'Steer this run…' : 'Message'} className="max-h-[220px] min-h-[40px] flex-1 resize-none bg-transparent px-1 py-2 text-[15px] leading-relaxed outline-none placeholder:text-muted" />
        <button type="submit" disabled={disabled} className="h-11 flex-none cursor-pointer rounded-xl bg-ink px-4 text-sm font-semibold text-on-ink hover:bg-ink-2 disabled:cursor-not-allowed disabled:opacity-50">Send</button>
      </div>
    </form>
  );
}

function StatusChip({ text, running }: { text: string; running: boolean }) {
  const tone = running ? 'bg-info-bg text-info' : 'bg-soft text-ink-2';
  return (
    <span role="status" className={`flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[11px] font-semibold ${tone}`}>
      {running && <span aria-hidden="true" className="live-dot h-1.5 w-1.5 rounded-full bg-info" />}
      {text}
    </span>
  );
}

export function SessionView({ sessionKey, agent, title }: { sessionKey: string; agent?: string; title: string }) {
  const { view, state, error, setError, approvalTick, send } = useSocket(sessionKey, agent);
  const abort = () => abortRun(sessionKey).catch((e: Error) => setError(e.message));
  const status = state === 'open' ? (view.running ? 'Running' : 'Idle') : state === 'reconnecting' ? 'Reconnecting' : 'Connecting';
  // Below lg the pane is the visible viewport above the tab bar and draws the only top bar, so the composer stays above the keyboard.
  const vv = useVisualViewportHeight();
  const keyboard = vv !== undefined && vv < window.innerHeight - 150;
  const chrome = keyboard ? '0px' : '58px - env(safe-area-inset-bottom, 0px)';
  const paneStyle = vv === undefined ? undefined : ({ '--ws-h': `calc(${vv}px - ${chrome})` } as CSSProperties);
  return (
    <div style={paneStyle} className="flex h-(--ws-h) min-h-0 flex-col lg:h-auto lg:flex-1">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-line bg-canvas/70 px-4 pt-[calc(0.5rem+env(safe-area-inset-top,0px))] pb-2 sm:px-5 lg:py-3">
        <a href="#/" aria-label="Sessions" className="grid h-11 w-11 flex-none place-items-center text-ink-2 no-underline hover:text-ink lg:hidden">
          <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M15 6l-6 6 6 6" /></svg>
        </a>
        <h1 className="m-0 min-w-0 flex-1 truncate text-[16px]" title={title}>{title}</h1>
        <StatusChip text={status} running={state === 'open' && view.running} />
        {state === 'open' && view.running && (
          <button type="button" onClick={abort} className="min-h-11 cursor-pointer rounded-lg border border-line bg-surface px-3 py-1 text-xs font-medium text-danger lg:min-h-0 hover:bg-danger-bg">Abort</button>
        )}
      </div>
      <div className="mx-auto flex min-h-0 w-full max-w-3xl min-w-0 flex-1 flex-col gap-4 overflow-y-auto px-4 [overflow-wrap:anywhere] [&_pre]:max-w-full [&_pre]:overflow-x-auto [&_pre]:whitespace-pre-wrap py-5 sm:px-5" aria-live="polite">
        {error && <p role="alert" className="m-0 rounded-xl border border-danger-line bg-danger-bg/60 px-3.5 py-2.5 text-[13px] text-danger">{error}</p>}
        <Transcript view={view} />
        <Approvals sessionKey={sessionKey} refresh={approvalTick} />
        <Artifacts sessionKey={sessionKey} refresh={approvalTick} />
      </div>
      <div className="border-t border-line bg-canvas/90 p-3 backdrop-blur-md sm:p-4">
        <div className="mx-auto max-w-3xl">
          <Composer busy={view.running} disabled={state !== 'open'} send={send} />
        </div>
      </div>
    </div>
  );
}

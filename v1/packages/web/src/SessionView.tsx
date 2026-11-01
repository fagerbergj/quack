import type { AgentEvent, SnapshotEvent } from '@earendil-works/pi-durable';
import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import { abortRun, socketUrl } from './api.ts';
import { Approvals } from './Approvals.tsx';
import { Artifacts } from './Artifacts.tsx';
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

function Composer({ busy, disabled, send }: { busy: boolean; disabled: boolean; send: (i: string, w?: 'steer' | 'followUp') => boolean }) {
  const [input, setInput] = useState('');
  const [mode, setMode] = useState<'steer' | 'followUp'>('steer');
  const submit = (e: SubmitEvent) => {
    e.preventDefault();
    if (!input.trim()) return;
    if (send(input, busy ? mode : undefined)) setInput('');
  };
  return (
    <form onSubmit={submit}>
      <label>
        Message
        <textarea value={input} onChange={(e) => setInput(e.target.value)} rows={3} />
      </label>
      {busy && (
        <fieldset>
          <legend>While the agent is working</legend>
          <label><input type="radio" name="mode" checked={mode === 'steer'} onChange={() => setMode('steer')} /> Steer</label>{' '}
          <label><input type="radio" name="mode" checked={mode === 'followUp'} onChange={() => setMode('followUp')} /> Follow up</label>
        </fieldset>
      )}
      <button type="submit" disabled={disabled}>Send</button>
    </form>
  );
}

export function SessionView({ sessionKey, agent }: { sessionKey: string; agent?: string }) {
  const { view, state, error, setError, approvalTick, send } = useSocket(sessionKey, agent);
  const abort = () => abortRun(sessionKey).catch((e: Error) => setError(e.message));
  return (
    <>
      <h1>{sessionKey}</h1>
      <p role="status">{state === 'open' ? (view.running ? 'Running' : 'Idle') : state === 'reconnecting' ? 'Reconnecting' : 'Connecting'}</p>
      {state === 'open' && view.running && <button type="button" onClick={abort}>Abort</button>}
      {error && <p role="alert">{error}</p>}
      <Transcript view={view} />
      <Approvals sessionKey={sessionKey} refresh={approvalTick} />
      <Artifacts sessionKey={sessionKey} refresh={approvalTick} />
      <Composer busy={view.running} disabled={state !== 'open'} send={send} />
    </>
  );
}

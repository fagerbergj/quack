import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import { AttachButton, Pending, useAttachments } from './Attach.tsx';
import { look } from './kinds.ts';
import { Pond } from './Pond.tsx';
import { Needs } from './Needs.tsx';
import { age, group, pendingInput, titleOf, type Row } from './sessions.ts';

const HOME_AGENT = 'quack';
// The New session buttons send this; Home answers by focusing its box.
// The one standing conversation; every Home send lands in it.
export const HOME_KEY = 'home:main';
export const FOCUS_ASK = 'quack:focus-ask';

// Same hand-off as the New session page: the text waits for the session's socket to open.
function Ask({ failed }: { failed: boolean }) {
  const [input, setInput] = useState('');
  const attach = useAttachments();
  const box = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    const focus = () => box.current?.focus();
    addEventListener(FOCUS_ASK, focus);
    return () => removeEventListener(FOCUS_ASK, focus);
  }, []);
  const send = (e: SubmitEvent) => {
    e.preventDefault();
    if (!input.trim() && !attach.images.length) return;
    pendingInput.set(HOME_KEY, { text: input, images: attach.images });
    location.hash = `#/s/${encodeURIComponent(HOME_KEY)}?agent=${HOME_AGENT}`;
  };
  return (
    <form onSubmit={send} {...attach.handlers} className="flex flex-col gap-2.5">
      <Pond still={failed}>
        <label htmlFor="ask" className="wordmark text-2xl leading-tight">What do you need?</label>
      </Pond>
      <div className="flex flex-col gap-2.5 rounded-[22px] border border-line-strong bg-surface py-3 pr-3 pl-4 shadow-float transition focus-within:border-ink-2/40 focus-within:shadow-ring">
        <textarea
          id="ask"
          ref={box}
          rows={3}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder="Ask about anything running, or start something new"
          className="resize-none bg-transparent text-[15.5px] leading-relaxed outline-none placeholder:text-muted"
        />
        <Pending attach={attach} />
        <div className="flex items-center justify-between">
        <AttachButton attach={attach} />
        <button type="submit" disabled={!input.trim() && !attach.images.length} aria-label="Send" className="grid size-11 cursor-pointer place-items-center rounded-full border-0 bg-ink text-on-ink hover:bg-ink-2 disabled:cursor-not-allowed disabled:opacity-50">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" aria-hidden="true"><path d="M12 19V5M6 11l6-6 6 6" /></svg>
        </button>
        </div>
      </div>
    </form>
  );
}

function SessionLink({ r, now }: { r: Row; now: number }) {
  const l = look(r);
  return (
    <a href={`#/s/${encodeURIComponent(r.key)}`} className="flex min-h-11 items-center gap-3 rounded-2xl border border-line bg-surface px-3.5 py-2.5 text-ink no-underline hover:bg-soft">
      <span aria-hidden="true" className="pulse-dot size-2 flex-none rounded-full bg-accent-ink" />
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate text-[14.5px] font-semibold">{titleOf(r)}</span>
        <span className="truncate text-[12.5px] text-muted">{[...new Set([l.label, l.line, age(r.createdAt, now)])].filter(Boolean).join(' · ')}</span>
      </span>
      <span aria-hidden="true" className="text-muted">›</span>
    </a>
  );
}

function Running({ rows, now }: { rows: Row[]; now: number }) {
  if (!rows.length) return null;
  return (
    <section aria-label="Running" className="flex flex-col gap-2 lg:hidden">
      <div className="flex items-baseline justify-between">
        <h2 className="m-0 font-sans text-xs font-semibold tracking-[.06em] text-muted uppercase">{`Running · ${rows.length}`}</h2>
        <a href="#/sessions" className="grid min-h-11 place-items-center text-[13.5px] text-link no-underline">All sessions</a>
      </div>
      {rows.map((r) => <SessionLink key={r.key} r={r} now={now} />)}
    </section>
  );
}

export function Home({ rows, error, now, reload }: { rows: Row[] | undefined; error: string; now: number; reload: () => void }) {
  const g = group(rows ?? []);
  return (
    <div className="mx-auto flex w-full max-w-[640px] flex-col gap-6 px-4 py-6 lg:py-12">
      <Ask failed={g.needs.some((r) => r.failures.length > 0)} />
      {rows?.some((r) => r.key === HOME_KEY) && <a href={`#/s/${encodeURIComponent(HOME_KEY)}`} className="grid min-h-11 place-items-center self-start text-[13.5px] text-link no-underline">Open conversation</a>}
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      <Needs rows={rows ?? []} now={now} reload={reload} />
      <Running rows={g.running} now={now} />
      <div className="feet w-full" aria-hidden="true" />
      {rows && !g.running.length && <a href="#/sessions" className="grid min-h-11 lg:hidden place-items-center self-start text-[13.5px] text-link no-underline">All sessions</a>}
    </div>
  );
}

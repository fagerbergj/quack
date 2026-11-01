import { useState, type SubmitEvent } from 'react';
import type { AgentSummary } from './api.ts';
import { Pond } from './Shell.tsx';
import { pendingInput, statusLine, titleOf, type Groups } from './sessions.ts';

const SUGGESTIONS = ['Review a pull request', 'Research a question'];

const pill = 'cursor-pointer rounded-full border border-line bg-surface px-3.5 py-1.5 text-[13px] text-ink-2 transition hover:border-line-strong hover:text-ink';

function Composer({ agents }: { agents: AgentSummary[] }) {
  const [agent, setAgent] = useState('');
  const [input, setInput] = useState('');
  const chosen = agent || agents[0]?.name || '';
  const start = (e: SubmitEvent) => {
    e.preventDefault();
    const key = `ui:${crypto.randomUUID()}`;
    if (input.trim()) pendingInput.set(key, input);
    location.hash = `#/s/${encodeURIComponent(key)}?agent=${encodeURIComponent(chosen)}`;
  };
  return (
    <div className="flex flex-col gap-5">
      <form onSubmit={start} className="rounded-[22px] border border-line-strong bg-surface shadow-float transition focus-within:border-ink-2/40 focus-within:shadow-ring">
        <div className="flex items-end gap-1.5 p-3">
          <label htmlFor="new-input" className="sr-only">Question or task</label>
          <textarea
            id="new-input"
            rows={3}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="Ask a question, paste a PR or issue URL, or describe a task"
            className="max-h-[220px] min-h-[40px] flex-1 resize-none bg-transparent px-1 py-2 text-[15.5px] leading-relaxed outline-none placeholder:text-muted"
          />
          <button type="submit" disabled={!chosen} aria-label="Start" className="grid h-10 w-10 flex-none cursor-pointer place-items-center rounded-xl bg-ink text-on-ink hover:bg-ink-2 disabled:cursor-not-allowed disabled:opacity-50">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M12 19V5M5 12l7-7 7 7" /></svg>
          </button>
        </div>
        <div className="flex flex-wrap items-center gap-2 border-t border-line px-3.5 py-2">
          <label htmlFor="agent-pick" className="text-[12px] text-muted">Agent</label>
          <select id="agent-pick" value={chosen} onChange={(e) => setAgent(e.target.value)} className="h-[28px] cursor-pointer rounded-lg border-0 bg-transparent px-1 text-[13px] font-medium text-ink outline-none hover:bg-soft">
            {agents.map((a) => (
              <option key={a.name} value={a.name}>{`${a.name} ${a.version}`}</option>
            ))}
          </select>
          <span className="ml-auto text-[12px] text-muted">Checked before anything leaves</span>
        </div>
      </form>
      <div className="flex flex-wrap gap-2">
        {SUGGESTIONS.map((s) => (
          <button key={s} type="button" className={pill} onClick={() => setInput(s)}>{s}</button>
        ))}
      </div>
    </div>
  );
}

function Waiting({ rows }: { rows: Groups['needs'] }) {
  if (!rows.length) return null;
  return (
    <section aria-labelledby="waiting-h" className="flex flex-col gap-3">
      <div className="flex items-baseline gap-2">
        <h3 id="waiting-h" className="m-0 text-sm font-semibold">Waiting on you</h3>
        <span className="text-[13px] text-muted">{rows.length}</span>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        {rows.map((r) => (
          <a key={r.key} href={`#/s/${encodeURIComponent(r.key)}`} className="flex items-start gap-3 rounded-[22px] border border-line bg-surface p-4 text-ink no-underline transition hover:-translate-y-px hover:border-line-strong hover:shadow-lift">
            <span aria-hidden="true" className="mt-0.5 grid h-8 w-8 flex-none place-items-center rounded-lg bg-warm text-warning">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M5 12l5 5L20 7" /></svg>
            </span>
            <span className="flex min-w-0 flex-col gap-1">
              <span className="truncate text-[14.5px] font-semibold">{titleOf(r)}</span>
              <span className="text-[13px] text-muted">{statusLine(r)}</span>
            </span>
          </a>
        ))}
      </div>
    </section>
  );
}

export function Landing({ agents, error, groups }: { agents: AgentSummary[]; error: string; groups: Groups }) {
  return (
    <div className="mx-auto flex w-full max-w-[720px] flex-col gap-8 px-4 py-8 sm:px-8 lg:py-12">
      <a href="#/" className="text-[13px] text-muted no-underline hover:text-ink lg:hidden">← Sessions</a>
      <Pond>
        <h2 className="m-0 text-[34px] leading-[1.1]">What should quack work on?</h2>
        <p className="m-0 mt-2 text-[14px] text-ink-2">{`${groups.needs.length} need you · ${groups.running.length} running`}</p>
      </Pond>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{`Agents: ${error}`}</p>}
      <Composer agents={agents} />
      <Waiting rows={groups.needs} />
    </div>
  );
}

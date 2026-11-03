import { useEffect, useState } from 'react';
import { getAgent, listAgents, promoteAgent, type AgentDetail, type AgentRubric, type AgentSummary } from './api.ts';
import { Modal, useAction } from './Housekeeping.tsx';
import { lineDiff, type Line } from './lineDiff.ts';
import { Markdown } from './Markdown.tsx';

const card = 'rounded-[18px] border border-line bg-surface p-3.5 shadow-card';
const label = 'm-0 mb-2 px-1 text-xs font-semibold tracking-wider text-muted uppercase';
const humanize = (key: string) => key.replace(/[_-]+/g, ' ').replace(/([a-z])([A-Z])/g, '$1 $2').toLowerCase().replace(/^./, (c) => c.toUpperCase());

function Status({ status }: { status: 'draft' | 'promoted' }) {
  const tone = status === 'promoted' ? 'bg-success-bg text-success' : 'bg-warning-bg text-warning';
  return <span className={`flex-none rounded-full px-2 py-0.5 text-[11px] font-semibold ${tone}`}>{status === 'promoted' ? 'Promoted' : 'Draft'}</span>;
}

function useLoad<T>(load: () => Promise<T>, deps: unknown[]): { value?: T; error?: string; reload: () => void } {
  const [state, setState] = useState<{ value?: T; error?: string }>({});
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    load().then((value) => live && setState({ value }), (e: Error) => live && setState({ error: e.message }));
    return () => { live = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);
  return { ...state, reload: () => setTick((t) => t + 1) };
}

export function AgentList() {
  const { value, error } = useLoad(listAgents, []);
  return (
    <div className="mx-auto max-w-3xl px-3 py-4 lg:py-8">
      <h1 className="m-0 mb-3 hidden px-1 text-[19px] lg:block">Agents</h1>
      {error && <p role="alert" className="px-1 text-sm text-danger">Could not load agents: {error}</p>}
      {value?.length === 0 && <p className="px-1 text-sm text-muted">No agents yet.</p>}
      <ul className="m-0 flex list-none flex-col gap-2.5 p-0">
        {value?.map((a: AgentSummary) => (
          <li key={a.name}>
            <a href={`#/agents/${encodeURIComponent(a.name)}`} className={`${card} flex min-h-11 flex-col gap-1.5 text-ink no-underline hover:bg-soft`}>
              <span className="flex items-center gap-2">
                <span className="min-w-0 flex-1 font-display text-base font-semibold [overflow-wrap:anywhere]">{a.name}</span>
                <Status status={a.status} />
              </span>
              {a.description && <span className="text-[13.5px] text-ink-2">{a.description}</span>}
              <span className="text-[12.5px] text-muted [overflow-wrap:anywhere]">{a.model ?? 'Default model'} · Version {a.version}</span>
            </a>
          </li>
        ))}
      </ul>
    </div>
  );
}

const HIDDEN = new Set(['name', 'version', 'status', 'model', 'description', 'prompt', 'rubric']);

function Value({ value }: { value: unknown }) {
  if (value && typeof value === 'object' && !Array.isArray(value)) {
    const entries = Object.entries(value);
    return entries.length ? <Fields entries={entries} nested /> : <span>Enabled</span>;
  }
  return <span className="[overflow-wrap:anywhere]">{Array.isArray(value) ? value.join(', ') : String(value)}</span>;
}

// Nested groups stack label over value: a second pair of columns inside the first is too narrow at phone width.
function Fields({ entries, nested }: { entries: Array<[string, unknown]>; nested?: boolean }) {
  return (
    <dl className={`m-0 flex flex-col gap-1.5 text-[13.5px] ${nested ? 'border-l border-line pl-3' : ''}`}>
      {entries.map(([k, v]) => (
        <div key={k} className={nested ? 'flex flex-col' : 'grid grid-cols-[minmax(0,2fr)_minmax(0,3fr)] gap-x-3'}>
          <dt className="text-muted">{humanize(k)}</dt>
          <dd className="m-0 min-w-0 text-ink"><Value value={v} /></dd>
        </div>
      ))}
    </dl>
  );
}

const rubricLines = (r?: AgentRubric) => (r?.criteria ?? []).flatMap((c) => [`## ${humanize(c.id)}`, ...c.checks.map((k) => `- ${k.label ?? 'Check'}${k.weight === 'gate' ? ' (gate)' : k.weight === undefined ? '' : ` (weight ${k.weight})`}`)]).join('\n');

function Rubric({ rubric }: { rubric?: AgentRubric }) {
  if (!rubric) return <p className="m-0 text-[13.5px] text-muted">This version has no rubric.</p>;
  return <div className="md text-[13.5px]"><Markdown text={rubricLines(rubric)} /></div>;
}

const row: Record<Line['kind'], string> = { same: 'text-ink-2', add: 'bg-success-bg text-success', del: 'bg-danger-bg text-danger', gap: 'text-muted' };
const mark: Record<Line['kind'], string> = { same: ' ', add: '+', del: '−', gap: '…' };
const kindName: Record<Line['kind'], string> = { same: '', add: 'Added: ', del: 'Removed: ', gap: 'Unchanged lines skipped' };

function Diff({ lines }: { lines: Line[] }) {
  if (!lines.length) return <p className="m-0 text-[13.5px] text-muted">No changes.</p>;
  return (
    <div className="overflow-hidden rounded-xl border border-line font-mono text-[12.5px] leading-[1.55]">
      {lines.map((l, i) => (
        <div key={i} className={`flex gap-2 px-2 py-0.5 ${row[l.kind]}`}>
          <span aria-hidden="true" className="w-3 flex-none select-none">{mark[l.kind]}</span>
          <span className="min-w-0 flex-1 whitespace-pre-wrap [overflow-wrap:anywhere]"><span className="sr-only">{kindName[l.kind]}</span>{l.text}</span>
        </div>
      ))}
    </div>
  );
}

function Changes({ name, shown, others }: { name: string; shown: AgentDetail; others: number[] }) {
  const [against, setAgainst] = useState<number | undefined>(others.find((v) => v < shown.version) ?? others[0]);
  const { value: base, error } = useLoad(() => (against === undefined ? Promise.resolve(undefined) : getAgent(name, against)), [name, against]);
  const select = 'min-h-11 rounded-xl border border-line bg-surface px-3 text-[15px] text-ink';
  return (
    <section className={`${card} flex flex-col gap-3`}>
      <label className="flex flex-col gap-1 text-[13px] text-muted">
        Compare version {shown.version} with
        <select value={against} onChange={(e) => setAgainst(Number(e.target.value))} className={select}>
          {others.map((v) => <option key={v} value={v}>Version {v}</option>)}
        </select>
      </label>
      {error && <p role="alert" className="m-0 text-sm text-danger">{error}</p>}
      {base && (
        <>
          <details className="group"><summary className="min-h-11 cursor-pointer py-2.5 text-[14px] font-medium">Prompt changes</summary><Diff lines={lineDiff(base.prompt, shown.prompt)} /></details>
          <details className="group"><summary className="min-h-11 cursor-pointer py-2.5 text-[14px] font-medium">Rubric changes</summary><Diff lines={lineDiff(rubricLines(base.rubric), rubricLines(shown.rubric))} /></details>
        </>
      )}
    </section>
  );
}

function PromoteDialog({ agent, onClose, onDone }: { agent: AgentDetail; onClose: () => void; onDone: () => void }) {
  const { busy, error, go } = useAction(() => promoteAgent(agent.name, agent.version), onDone);
  return (
    <Modal title={`Promote ${agent.name}?`} onClose={onClose} onSubmit={go} action="Promote" busy={busy} error={error}>
      <p className="m-0 text-[14px]">Version {agent.version} will be allowed to run unattended, such as from webhooks.</p>
    </Modal>
  );
}

export function AgentView({ name }: { name: string }) {
  const [version, setVersion] = useState<number>();
  const [confirming, setConfirming] = useState(false);
  const { value: agent, error, reload } = useLoad(() => getAgent(name, version), [name, version]);
  const current = agent && Math.max(...agent.versions.map((v) => v.version));
  const model = agent?.definition.model;
  const description = agent?.definition.description;
  const settings = agent ? Object.entries(agent.definition).filter(([k]) => !HIDDEN.has(k)) : [];
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-3 px-3 py-4 lg:py-8">
      <a href="#/agents" className="inline-flex min-h-11 items-center self-start text-[14px] text-link no-underline">‹ Agents</a>
      {error && <p role="alert" className="m-0 px-1 text-sm text-danger">Could not load {name}: {error}</p>}
      {agent && (
        <>
          <header className="flex flex-col gap-1.5 px-1">
            <div className="flex items-center gap-2">
              <h1 className="m-0 min-w-0 flex-1 text-[19px] [overflow-wrap:anywhere]">{agent.name}</h1>
              <Status status={agent.status} />
            </div>
            {typeof description === 'string' && <p className="m-0 text-[14px] text-ink-2">{description}</p>}
            <p className="m-0 text-[12.5px] text-muted [overflow-wrap:anywhere]">{typeof model === 'string' ? model : 'Default model'} · Version {agent.version}</p>
            {agent.status === 'draft' && agent.version === current && (
              <button type="button" onClick={() => setConfirming(true)} className="mt-1 min-h-11 cursor-pointer self-start rounded-full border-0 bg-ink px-5 text-[14px] font-medium text-on-ink hover:bg-ink-2">Promote</button>
            )}
          </header>
          <section className={card}>
            <details>
              <summary className="min-h-11 cursor-pointer py-2.5 text-[15px] font-semibold">Prompt</summary>
              <div className="md pb-2 text-[14px]"><Markdown text={agent.prompt} /></div>
            </details>
          </section>
          <section className={card}>
            <h2 className="m-0 mb-2 font-sans text-[15px] font-semibold">Rubric</h2>
            <Rubric rubric={agent.rubric} />
          </section>
          {settings.length > 0 && (
            <section className={`${card} flex flex-col gap-2`}>
              <h2 className="m-0 font-sans text-[15px] font-semibold">Settings</h2>
              <Fields entries={settings} />
            </section>
          )}
          <section>
            <h2 className={label}>Versions</h2>
            <div className="flex flex-wrap gap-2">
              {agent.versions.map((v) => (
                <button key={v.version} type="button" aria-pressed={v.version === agent.version} onClick={() => setVersion(v.version)} className="min-h-11 min-w-11 cursor-pointer rounded-full border border-line bg-surface px-4 text-[14px] text-ink aria-pressed:border-accent aria-pressed:bg-soft aria-pressed:font-semibold">
                  Version {v.version}{v.status === 'promoted' ? ' · promoted' : ''}
                </button>
              ))}
            </div>
          </section>
          {agent.versions.length > 1 && <Changes key={agent.version} name={name} shown={agent} others={agent.versions.map((v) => v.version).filter((v) => v !== agent.version)} />}
          {confirming && <PromoteDialog agent={agent} onClose={() => setConfirming(false)} onDone={() => (setConfirming(false), reload())} />}
        </>
      )}
    </div>
  );
}

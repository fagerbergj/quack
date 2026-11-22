import { useState } from 'react';
import { getAgent, listAgents, promoteAgent, type AgentDetail, type AgentRubric, type AgentSummary } from './api.ts';
import { Modal, useAction } from './Housekeeping.tsx';
import { lineDiff, type Line } from './lineDiff.ts';
import { Markdown } from './Markdown.tsx';
import { StatusMark, VersionMark } from './StatusIcon.tsx';
import { useLoad } from './useLoad.ts';

const card = 'rounded-[18px] border border-line bg-surface p-3.5 shadow-card';
const label = 'm-0 mb-2 px-1 text-xs font-semibold tracking-wider text-muted uppercase';
const humanize = (key: string) => key.replace(/[_-]+/g, ' ').replace(/([a-z])([A-Z])/g, '$1 $2').toLowerCase().replace(/^./, (c) => c.toUpperCase());

const MARK = { promoted: { mark: 'ok', label: 'Promoted: runs unattended' }, draft: { mark: 'draft', label: 'Draft' } } as const;
const markOf = (status: 'draft' | 'promoted') => MARK[status];

// Promoted first, then by name.
const order = (x: AgentSummary, y: AgentSummary) => Number(y.status === 'promoted') - Number(x.status === 'promoted') || x.name.localeCompare(y.name);
const prefixOf = (name: string) => (name.includes('-') ? name.slice(0, name.indexOf('-')) : '');

/** Agents sharing a name prefix (sleeper-digest, sleeper-retro) form a group; the rest list first, ungrouped. */
export function agentGroups(agents: AgentSummary[]): Array<{ prefix: string; agents: AgentSummary[] }> {
  const by = Map.groupBy(agents.toSorted(order), (a) => prefixOf(a.name));
  const loose = [...by].flatMap(([p, xs]) => (p && xs.length > 1 ? [] : xs)).toSorted(order);
  return [{ prefix: '', agents: loose }, ...[...by].filter(([p, xs]) => p && xs.length > 1).map(([prefix, xs]) => ({ prefix, agents: xs }))].filter((g) => g.agents.length);
}

// The model shows once in the lead when every agent shares it, otherwise on each row; the spaces only part the accessible name.
function AgentRow({ a, prefix, model }: { a: AgentSummary; prefix: string; model: boolean }) {
  const { mark, label } = markOf(a.status);
  return (
    <li>
      <a href={`#/agents/${encodeURIComponent(a.name)}`} className="flex min-h-12 items-center gap-3 px-4 py-2 text-ink no-underline hover:bg-soft">
        <span className="flex min-w-0 flex-1 flex-col">
          <span className="text-[14.5px] font-semibold [overflow-wrap:anywhere]">{prefix && <span className="font-normal text-muted">{`${prefix}-`}</span>}{a.name.slice(prefix ? prefix.length + 1 : 0)}</span>
          {a.description && ' '}
          {a.description && <span className="truncate text-[12.5px] text-muted">{a.description}</span>}
        </span>{' '}
        {model && <><span className="hidden flex-none rounded-md border border-line bg-soft px-1.5 py-0.5 font-mono text-[11px] text-ink-2 sm:inline">{a.model ?? DEFAULT}</span>{' '}</>}
        <VersionMark version={a.version} mark={mark} label={label} />
        <span aria-hidden="true" className="flex-none text-muted">›</span>
      </a>
    </li>
  );
}

const DEFAULT = 'Default model';
const list = 'm-0 flex list-none flex-col divide-y divide-line p-0';
const head = 'm-0 flex items-baseline gap-2 border-y border-line bg-soft px-4 py-2 font-sans text-xs font-semibold tracking-wider text-muted uppercase';

export function AgentList() {
  const { data: value, error } = useLoad(listAgents, [], undefined);
  const models = new Set(value?.map((a) => a.model ?? DEFAULT));
  const shared = models.size === 1 ? [...models][0] : undefined;
  const promoted = value?.filter((a) => a.status === 'promoted').length ?? 0;
  return (
    <div className="mx-auto max-w-3xl px-4 py-4 lg:py-8">
      <h1 className="m-0 mb-3 hidden text-[19px] lg:block">Agents</h1>
      {error && <p role="alert" className="px-1 text-sm text-danger">Could not load agents: {error}</p>}
      {value?.length === 0 && <p className="px-1 text-sm text-muted">No agents yet.</p>}
      {!!value?.length && (
        <p className="m-0 mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 px-1 text-[13px] text-ink-2">
          <span>{`${value.length} agents`}</span>
          {promoted > 0 && <><span aria-hidden="true" className="text-muted">·</span><span className="inline-flex items-center gap-1.5">{`${promoted} promoted`}<StatusMark mark="ok" label="Promoted: runs unattended" /></span></>}
          {shared && <><span aria-hidden="true" className="text-muted">·</span><span>all on {shared === DEFAULT ? 'the default model' : <span className="rounded-md border border-line bg-soft px-1.5 py-0.5 font-mono text-[11.5px]">{shared}</span>}</span></>}
        </p>
      )}
      <div className="overflow-hidden rounded-[18px] border border-line bg-surface shadow-card empty:hidden">
        {value && agentGroups(value).map((g) => (
          <section key={g.prefix} aria-label={g.prefix ? `${g.prefix} agents` : 'Agents'}>
            {g.prefix && <h2 className={head}>{g.prefix}<span className="font-normal tracking-normal normal-case">{`${g.agents.length} agents`}</span></h2>}
            <ul className={list}>{g.agents.map((a) => <AgentRow key={a.name} a={a} prefix={g.prefix} model={!shared} />)}</ul>
          </section>
        ))}
      </div>
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
  const { data: base, error } = useLoad(() => (against === undefined ? Promise.resolve(undefined) : getAgent(name, against)), [name, against], undefined);
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

export function AgentsRoute({ hash }: { hash: string }) {
  const name = /^#\/agents\/([^/?]+)$/.exec(hash)?.[1];
  // A malformed escape in the hash falls back to the list rather than throwing.
  const decoded = name && (() => { try { return decodeURIComponent(name); } catch { return undefined; } })();
  return decoded ? <AgentView name={decoded} /> : <AgentList />;
}

export function AgentView({ name }: { name: string }) {
  const [version, setVersion] = useState<number>();
  const [confirming, setConfirming] = useState(false);
  const { data: agent, error, reload } = useLoad<AgentDetail | undefined>(() => getAgent(name, version), [name, version], undefined);
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
              <VersionMark version={agent.version} {...markOf(agent.status)} />
            </div>
            {typeof description === 'string' && <p className="m-0 text-[14px] text-ink-2">{description}</p>}
            <p className="m-0 text-[12.5px] text-muted [overflow-wrap:anywhere]">{typeof model === 'string' ? model : 'Default model'}</p>
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
                  <VersionMark version={v.version} {...markOf(v.status)} />
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

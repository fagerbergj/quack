import { useEffect, useState, type SubmitEvent } from 'react';
import { listAgents, listSessions, type AgentSummary, type SessionSummary } from './api.ts';
import { SessionView } from './SessionView.tsx';
import { Shell, type Route } from './Shell.tsx';

// shortcut: hash routing keeps the build static, so any file server can host it; move to a router with auth.
function safeDecode(s: string): string | undefined {
  try {
    return decodeURIComponent(s);
  } catch {
    return undefined;
  }
}

function parseHash(hash: string): { key: string; agent?: string } | undefined {
  const m = /^#\/s\/([^?]+)(?:\?agent=(.*))?$/.exec(hash);
  const key = m && safeDecode(m[1]);
  return m && key ? { key, agent: m[2] ? safeDecode(m[2]) : undefined } : undefined;
}

function useHash() {
  const [hash, setHash] = useState(location.hash);
  useEffect(() => {
    const on = () => setHash(location.hash);
    addEventListener('hashchange', on);
    return () => removeEventListener('hashchange', on);
  }, []);
  return hash;
}

function NewSession({ agents }: { agents: AgentSummary[] }) {
  const [agent, setAgent] = useState('');
  const chosen = agent || agents[0]?.name || '';
  const start = (e: SubmitEvent) => {
    e.preventDefault();
    location.hash = `#/s/${encodeURIComponent(`ui:${crypto.randomUUID()}`)}?agent=${encodeURIComponent(chosen)}`;
  };
  return (
    <form onSubmit={start} className="flex flex-wrap items-center gap-2">
      <label className="flex items-center gap-2 text-sm">
        Agent
        <select className="h-8 rounded-full border border-line bg-soft px-2 text-sm" value={chosen} onChange={(e) => setAgent(e.target.value)}>
          {agents.map((a) => (
            <option key={a.name} value={a.name}>{`${a.name} ${a.version}`}</option>
          ))}
        </select>
      </label>
      <button type="submit" disabled={!chosen} className="h-8 cursor-pointer rounded-full bg-ink px-3 text-sm font-semibold text-on-ink hover:bg-ink-2 disabled:cursor-not-allowed disabled:opacity-50">New session</button>
    </form>
  );
}

function Home() {
  const [sessions, setSessions] = useState<SessionSummary[]>();
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  const [sessionsError, setSessionsError] = useState('');
  const [agentsError, setAgentsError] = useState('');
  useEffect(() => {
    listSessions().then(setSessions, (e: Error) => setSessionsError(e.message));
    listAgents().then(setAgents, (e: Error) => setAgentsError(e.message));
  }, []);
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4 px-4 py-6 sm:px-6">
      <h1 className="m-0 text-2xl">Sessions</h1>
      {sessionsError && <p role="alert">{`Sessions: ${sessionsError}`}</p>}
      {agentsError && <p role="alert">{`Agents: ${agentsError}`}</p>}
      <NewSession agents={agents} />
      {sessions?.length === 0 && <p>No sessions yet.</p>}
      <ul className="m-0 flex list-none flex-col p-0">
        {sessions?.map((s) => (
          <li key={s.key} className="flex flex-wrap items-baseline gap-x-2 border-b border-line py-2 text-sm text-ink-2">
            <a href={`#/s/${encodeURIComponent(s.key)}`}>{s.key}</a> {s.agent} {s.running ? 'running' : 'idle'}{' '}
            <time dateTime={s.lastActivity}>{new Date(s.lastActivity).toLocaleString()}</time>
          </li>
        ))}
      </ul>
    </div>
  );
}

const PLACEHOLDERS: Partial<Record<Route, string>> = {
  inbox: 'Inbox', artifacts: 'Artifacts', sleeper: 'Sleeper', remarkable: 'reMarkable', memory: 'Memory', agents: 'Agents', evals: 'Evals', schedules: 'Schedules',
};

function navRoute(hash: string): Route {
  const name = /^#\/(\w+)\/?$/.exec(hash)?.[1] as Route | undefined;
  return name && name in PLACEHOLDERS ? name : 'sessions';
}

function Placeholder({ title }: { title: string }) {
  return (
    <div className="mx-auto max-w-3xl px-4 py-8 sm:px-6">
      <h1 className="m-0 text-2xl">{title}</h1>
      <p className="text-sm text-muted">Nothing here yet.</p>
    </div>
  );
}

export function App() {
  const hash = useHash();
  const session = parseHash(hash);
  const nav = navRoute(hash);
  const title = !session && PLACEHOLDERS[nav];
  return (
    <Shell route={nav}>
      {session ? (
        <div className="mx-auto max-w-3xl px-4 py-6 sm:px-6">
          <a href="#/">All sessions</a>
          <SessionView key={session.key} sessionKey={session.key} agent={session.agent} />
        </div>
      ) : title ? (
        <Placeholder title={title} />
      ) : (
        <Home />
      )}
    </Shell>
  );
}

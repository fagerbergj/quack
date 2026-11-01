import { useEffect, useState } from 'react';
import { listAgents, listSessions, type AgentSummary, type SessionSummary } from './api.ts';
import { SessionView } from './SessionView.tsx';

// shortcut: hash routing keeps the build static, so any file server can host it; move to a router with auth.
function parseHash(hash: string): { key: string; agent?: string } | undefined {
  const m = /^#\/s\/([^?]+)(?:\?agent=(.*))?$/.exec(hash);
  return m ? { key: decodeURIComponent(m[1]), agent: m[2] ? decodeURIComponent(m[2]) : undefined } : undefined;
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
  const start = (e: React.FormEvent) => {
    e.preventDefault();
    location.hash = `#/s/${encodeURIComponent(`ui:${crypto.randomUUID()}`)}?agent=${encodeURIComponent(chosen)}`;
  };
  return (
    <form onSubmit={start}>
      <label>
        Agent{' '}
        <select value={chosen} onChange={(e) => setAgent(e.target.value)}>
          {agents.map((a) => (
            <option key={a.name} value={a.name}>{`${a.name} ${a.version}`}</option>
          ))}
        </select>
      </label>{' '}
      <button type="submit" disabled={!chosen}>New session</button>
    </form>
  );
}

function Home() {
  const [sessions, setSessions] = useState<SessionSummary[]>();
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  const [error, setError] = useState('');
  useEffect(() => {
    listSessions().then(setSessions, (e: Error) => setError(e.message));
    listAgents().then(setAgents, (e: Error) => setError(e.message));
  }, []);
  return (
    <>
      <h1>Sessions</h1>
      {error && <p role="alert">{error}</p>}
      <NewSession agents={agents} />
      {sessions?.length === 0 && <p>No sessions yet.</p>}
      <ul>
        {sessions?.map((s) => (
          <li key={s.key}>
            <a href={`#/s/${encodeURIComponent(s.key)}`}>{s.key}</a> {s.agent} {s.running ? 'running' : 'idle'}{' '}
            <time dateTime={new Date(s.lastActivity).toISOString()}>{new Date(s.lastActivity).toLocaleString()}</time>
          </li>
        ))}
      </ul>
    </>
  );
}

export function App() {
  const route = parseHash(useHash());
  return (
    <main>
      {route ? (
        <>
          <a href="#/">All sessions</a>
          <SessionView key={route.key} sessionKey={route.key} agent={route.agent} />
        </>
      ) : (
        <Home />
      )}
    </main>
  );
}

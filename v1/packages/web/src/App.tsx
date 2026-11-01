import { useEffect, useState } from 'react';
import { Shell, type Route } from './Shell.tsx';
import { SessionList } from './SessionList.tsx';
import { useDesktop } from './useDesktop.ts';
import { useWorkspaceData, Workspace } from './Workspace.tsx';

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
  const raw = useHash();
  const hash = raw !== '#/' && raw.endsWith('/') ? raw.slice(0, -1) : raw;
  const session = parseHash(hash);
  const nav = navRoute(hash);
  const title = !session && PLACEHOLDERS[nav];
  const data = useWorkspaceData(session?.key);
  const desktop = useDesktop();
  const list = <SessionList rows={data.rows} selected={session?.key} error={data.error} now={data.now} compact={desktop} />;
  return (
    <Shell route={nav} bare={Boolean(session)} sidebar={desktop ? list : undefined}>
      {title ? <Placeholder title={title} /> : <Workspace data={data} selected={session} isNew={hash === '#/new'} list={desktop ? undefined : list} />}
    </Shell>
  );
}

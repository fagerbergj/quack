import { useEffect, useState } from 'react';
import { AgentsRoute } from './Agents.tsx';
import { ArtifactPage } from './ArtifactPage.tsx';
import { ArtifactList } from './ArtifactList.tsx';
import { FOCUS_ASK } from './Home.tsx';
import { More } from './More.tsx';
import { Shell, type Route } from './Shell.tsx';
import { Settings } from './Settings.tsx';
import { SessionList } from './SessionList.tsx';
import { EXTENSIONS } from './extensions/index.ts';
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

// An artifact's own page: #/s/<key>/spec, with ?v=<version> to pick one.
function parseFull(hash: string): { key: string; version?: number } | undefined {
  const m = /^#\/s\/([^/?]+)\/spec(?:\?v=(\d+))?$/.exec(hash);
  const key = m && safeDecode(m[1]!);
  return m && key ? { key, version: m[2] ? Number(m[2]) : undefined } : undefined;
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

const NAVS = EXTENSIONS.flatMap((e) => e.nav ?? []);
const PLACEHOLDERS: Record<Route, string> = {
  more: 'More', artifacts: 'Artifacts', agents: 'Agents', settings: 'Settings',
  ...Object.fromEntries(NAVS.map((n) => [n.route, n.label])),
};

function navRoute(hash: string): Route {
  if (hash === '#/' || hash === '' || hash === '#/new') return 'home';
  const name = /^#\/(\w+)(?:\/[^/?]+)?\/?$/.exec(hash)?.[1];
  return name && Object.hasOwn(PLACEHOLDERS, name) ? name : 'sessions';
}

function Placeholder({ title }: { title: string }) {
  return (
    <div className="mx-auto max-w-3xl px-4 py-8 sm:px-6">
      <h1 className="m-0 text-2xl">{title}</h1>
      <p className="text-sm text-muted">Nothing here yet.</p>
    </div>
  );
}

const trimSlash = (h: string) => (h !== '#/' && h.endsWith('/') ? h.slice(0, -1) : h);

// On desktop the list lives in the sidebar, except on #/sessions itself, where it is the page.
const listInSidebar = (desktop: boolean, nav: Route, open: boolean) => desktop && (open || nav !== 'sessions');

export function App() {
  const raw = useHash();
  const hash = trimSlash(raw);
  const full = parseFull(hash);
  const session = full ?? parseHash(hash);
  // Inbox folded into Home; old links still land.
  useEffect(() => {
    if (hash === '#/inbox') location.replace('#/');
    if (hash === '#/new') {
      location.replace('#/');
      dispatchEvent(new Event(FOCUS_ASK));
    }
  }, [hash]);
  const nav = session?.key.startsWith('home:') ? 'home' : session ? 'sessions' : navRoute(hash);
  const Page = session ? undefined : NAVS.find((n) => n.route === nav)?.page;
  const title = !session && PLACEHOLDERS[nav];
  const data = useWorkspaceData(session?.key);
  const desktop = useDesktop();
  const list = <SessionList rows={data.rows} selected={session?.key} error={data.error} now={data.now} />;
  const inSidebar = listInSidebar(desktop, nav, Boolean(session));
  return (
    <Shell route={nav} bare={Boolean(session)} sidebar={inSidebar ? list : undefined}>
      {full ? <ArtifactPage sessionKey={full.key} version={full.version} />
        : Page ? <Page rows={data.rows} />
        : nav === 'more' ? <More />
        : nav === 'settings' ? <Settings />
        : nav === 'agents' ? <AgentsRoute hash={hash} />
        : nav === 'artifacts' ? <ArtifactList rows={data.rows} now={data.now} />
        : title ? <Placeholder title={title} /> : <Workspace data={data} selected={session} home={nav === 'home'} list={inSidebar ? undefined : list} />}
    </Shell>
  );
}

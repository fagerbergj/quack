import { useEffect, useState } from 'react';
import { AgentsRoute } from './Agents.tsx';
import { ArtifactPage, parseArtifactRoute } from './ArtifactPage.tsx';
import { ArtifactList } from './ArtifactList.tsx';
import { More } from './More.tsx';
import { Shell, type Route } from './Shell.tsx';
import { Settings } from './Settings.tsx';
import { SessionList } from './SessionList.tsx';
import { EXTENSIONS } from './extensions/index.ts';
import { ExtensionProviders } from './slots.tsx';
import { useDesktop } from './useDesktop.ts';
import { useUpdates } from './useUpdates.ts';
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

const NAVS = EXTENSIONS.flatMap((e) => e.nav ?? []);
const ROUTES = new Set<Route>(['more', 'artifacts', 'agents', 'settings', ...NAVS.map((n) => n.route)]);

function navRoute(hash: string): Route {
  if (hash === '#/' || hash === '') return 'home';
  const name = /^#\/(\w+)(?:\/[^/?]+)?\/?$/.exec(hash)?.[1];
  return name && ROUTES.has(name) ? name : 'sessions';
}

// A session's page sits under Home when it is Home's chat, else under Sessions.
const sessionRoute = (key?: string): Route | undefined => (key === undefined ? undefined : key.startsWith('home:') ? 'home' : 'sessions');

const trimSlash = (h: string) => (h !== '#/' && h.endsWith('/') ? h.slice(0, -1) : h);

// On desktop the list lives in the sidebar, except on #/sessions itself, where it is the page.
const listInSidebar = (desktop: boolean, nav: Route, open: boolean) => desktop && (open || nav !== 'sessions');

export function App() {
  const raw = useHash();
  const hash = trimSlash(raw);
  const full = parseArtifactRoute(hash);
  const session = full ?? parseHash(hash);
  useUpdates();
  const nav = full ? 'artifacts' : sessionRoute(session?.key) ?? navRoute(hash);
  const Page = session ? undefined : NAVS.find((n) => n.route === nav)?.page;
  const data = useWorkspaceData(session?.key);
  const desktop = useDesktop();
  const list = <SessionList rows={data.rows} selected={session?.key} error={data.error} now={data.now} />;
  const inSidebar = listInSidebar(desktop, nav, Boolean(session));
  return (
    <ExtensionProviders rows={data.rows}>
      <Shell route={nav} bare={Boolean(session)} sidebar={inSidebar ? list : undefined}>
        {full ? <ArtifactPage sessionKey={full.key} kind={full.kind} version={full.version} rows={data.rows} />
          : Page ? <Page rows={data.rows} />
          : nav === 'more' ? <More />
          : nav === 'settings' ? <Settings />
          : nav === 'agents' ? <AgentsRoute hash={hash} />
          : nav === 'artifacts' ? <ArtifactList rows={data.rows} now={data.now} />
          : <Workspace data={data} selected={session} home={nav === 'home'} list={inSidebar ? undefined : list} />}
      </Shell>
    </ExtensionProviders>
  );
}

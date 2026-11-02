import { useCallback, useEffect, useState } from 'react';
import type { Row } from '../../sessions.ts';
import { documentTree, ingest, notebooks, type Entry, type State } from './api.ts';

const CHIPS: Record<State, { label: string; cls: string }> = {
  changed: { label: 'Changed', cls: 'bg-warning-bg text-warning' },
  new: { label: 'New', cls: 'bg-success-bg text-success' },
  failed: { label: 'Ingest failed', cls: 'bg-danger-bg text-danger' },
  ingesting: { label: 'Ingesting', cls: 'bg-info-bg text-info' },
  current: { label: 'Up to date', cls: 'bg-soft text-muted' },
};

const day = (iso: string) => (iso ? new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' }).format(new Date(iso)) : '');
const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`;

// Folders are found by walking the tree with the ids in the breadcrumb.
function walk(tree: Entry[], path: Entry[]): Entry[] {
  return path.length ? (path[path.length - 1].children ?? []) : tree;
}

function Swimmer({ className }: { className?: string }) {
  return (
    <svg width="34" height="26" viewBox="0 0 64 48" aria-hidden="true" className={className}>
      <ellipse cx="34" cy="32" rx="22" ry="11" fill="#fdfcf7" />
      <circle cx="20" cy="18" r="10" fill="#1f7a63" />
      <rect x="11" y="25" width="18" height="3" rx="1.5" fill="#fff" />
      <path d="M12 15 L1 19 L12 22 Z" fill="#e3a92b" />
      <circle cx="17" cy="15" r="1.8" fill="#0b0f0e" />
    </svg>
  );
}

const SCRIBBLES = [
  'M2 6 C10 1 16 9 24 4 S38 2 46 6 S60 3 70 6M2 20 C12 15 20 23 30 18 S46 16 56 20 S78 17 92 20M2 34 C10 30 18 37 26 32 S40 30 48 34M2 48 C14 43 22 52 34 46 S54 44 66 48 S84 45 98 48M2 62 C10 58 20 65 28 60 S44 58 54 62',
  'M48 30 C60 20 70 40 84 28M14 66 C26 60 34 70 46 64 S66 62 78 66M10 14 C22 8 30 20 44 12',
];

function Paper({ id, dim }: { id: string; dim: boolean }) {
  const scribble = SCRIBBLES[id.charCodeAt(id.length - 1) % SCRIBBLES.length];
  return (
    <span aria-hidden="true" className={`paper relative block h-32 overflow-hidden rounded-[6px_12px_12px_6px] ${dim ? 'opacity-80' : ''}`}>
      <svg className="absolute top-4 left-3.5" width="110" height="84" viewBox="0 0 110 84" fill="none" stroke="#2f3a36" strokeWidth="1.6" strokeLinecap="round"><path d={scribble} /></svg>
    </span>
  );
}

function Folder({ entry, open }: { entry: Entry; open: () => void }) {
  const n = notebooks(entry.children ?? []).length;
  return (
    <button type="button" onClick={open} className="flex min-h-11 w-full cursor-pointer flex-col gap-2 rounded-[18px] border border-line bg-surface p-2.5 text-left text-ink shadow-card">
      <span aria-hidden="true" className="relative block h-32">
        <span className="absolute inset-[10px_6px_0_14px] rotate-[4deg] rounded-[6px_10px_10px_10px] bg-line-strong" />
        <span className="absolute inset-[6px_10px_2px_8px] -rotate-3 rounded-[6px_10px_10px_10px] bg-line" />
        <span className="absolute top-[-2px] left-2.5 h-3.5 w-11 rounded-t-md bg-[#3d8fb5]" />
        <span className="font-display absolute inset-[10px_8px_6px_10px] grid place-items-center rounded-[4px_10px_10px_10px] bg-link text-[26px] font-semibold text-on-ink">{n}</span>
      </span>
      <span className="text-[14.5px] font-semibold break-words">{entry.name}</span>
      <span className="-mt-1.5 text-[12.5px] text-muted">Folder · {plural(n, 'notebook')}</span>
    </button>
  );
}

function Notebook({ entry, on, toggle }: { entry: Entry; on: boolean; toggle: () => void }) {
  const chip = CHIPS[entry.state ?? 'current'];
  return (
    <div>
      <label className={`relative flex cursor-pointer flex-col gap-2 rounded-[18px] border-2 p-2.5 ${on ? 'border-accent-ink bg-success-bg/40' : 'border-line bg-surface'}`}>
        <input type="checkbox" checked={on} onChange={toggle} aria-label={`Select ${entry.name}`} className="absolute inset-0 z-10 m-0 h-full w-full cursor-pointer opacity-0" />
        <Paper id={entry.id} dim={entry.state === 'current'} />
        <span aria-hidden="true" className={`absolute top-4 right-4 grid size-[22px] place-items-center rounded-full border-2 ${on ? 'border-accent-ink bg-accent-ink text-on-ink' : 'border-line-strong bg-surface'}`}>
          {on && <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" strokeLinecap="round" strokeLinejoin="round"><path d="M5 12l5 5L20 7" /></svg>}
        </span>
        <span className="text-[14.5px] font-semibold break-words">{entry.name}</span>
        <span className="-mt-1.5 flex items-center gap-1.5">
          <span className={`rounded-full px-2 py-px text-xs ${chip.cls}`}>{chip.label}</span>
          <span className="text-xs text-muted">{day(entry.modified)}</span>
        </span>
      </label>
      {entry.state === 'failed' && entry.error && (
        <details className="mt-1 px-2">
          <summary className="flex min-h-11 cursor-pointer items-center text-[13px] text-muted">Details</summary>
          <p className="m-0 pb-2 text-xs break-words whitespace-pre-wrap text-muted">{entry.error}</p>
        </details>
      )}
    </div>
  );
}

function Crumbs({ path, go }: { path: Entry[]; go: (depth: number) => void }) {
  const chip = 'inline-flex h-[30px] items-center rounded-full px-3 text-[13px]';
  return (
    <nav aria-label="Folder" className="flex flex-wrap items-center gap-x-1.5 px-4 pb-3">
      {[{ id: '', name: 'My files' }, ...path].map((e, i, all) => (
        <span key={e.id} className="flex items-center gap-1.5">
          {i > 0 && <span aria-hidden="true" className="text-line-strong">/</span>}
          {i === all.length - 1 ? (
            <span aria-current="page" className={`${chip} bg-line font-semibold text-ink`}>{e.name}</span>
          ) : (
            <button type="button" onClick={() => go(i)} className="flex h-11 cursor-pointer items-center border-0 bg-transparent p-0"><span className={`${chip} bg-soft text-ink-2`}>{e.name}</span></button>
          )}
        </span>
      ))}
    </nav>
  );
}

function useTree() {
  const [tree, setTree] = useState<Entry[]>();
  const [error, setError] = useState('');
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const load = useCallback(async (preselect: boolean) => {
    try {
      const t = await documentTree();
      setTree(t);
      setError('');
      if (preselect) setPicked(new Set(notebooks(t).filter((n) => n.state === 'changed').map((n) => n.id)));
    } catch (e) {
      setError((e as Error).message === 'no such app' ? 'reMarkable is not connected.' : (e as Error).message);
    }
  }, []);
  useEffect(() => void load(true), [load]);
  return { tree, error, picked, setPicked, reload: () => load(false) };
}

export function Remarkable(_: { rows: Row[] | undefined }) {
  const { tree, error, picked, setPicked, reload } = useTree();
  const [path, setPath] = useState<Entry[]>([]);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState('');
  const here = tree ? walk(tree, path) : [];
  const changed = tree ? notebooks(tree).filter((n) => n.state === 'changed').length : 0;
  const toggle = (id: string) => setPicked((s) => { const next = new Set(s); if (!next.delete(id)) next.add(id); return next; });
  const start = async () => {
    setBusy(true);
    setFailure('');
    try {
      await ingest([...picked]);
      setPicked(new Set());
      await reload();
    } catch (e) {
      setFailure((e as Error).message);
    }
    setBusy(false);
  };
  return (
    <div className="mx-auto max-w-3xl pt-3 pb-44 lg:pt-8">
      <header className="flex items-center gap-2.5 px-4 pb-2">
        <span className="grid size-[34px] flex-none place-items-center rounded-[10px] bg-soft text-ink-2" aria-hidden="true">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M4 20h4L19 9l-4-4L4 16Z" /><path d="M13.5 6.5l4 4" /></svg>
        </span>
        <div className="min-w-0">
          <h1 className="m-0 text-[19px] leading-tight">reMarkable</h1>
          {tree && <p className="m-0 text-[12.5px] text-muted">{changed ? `${changed} changed since last ingest` : 'Everything is up to date'}</p>}
        </div>
        <button type="button" onClick={() => void reload()} className="ml-auto min-h-11 cursor-pointer rounded-lg border-0 bg-transparent px-3 text-[13px] text-muted hover:bg-soft hover:text-ink">Refresh</button>
      </header>
      {error && <p role="alert" className="mx-4 text-sm text-danger">{error}</p>}
      {tree && (
        <>
          <Crumbs path={path} go={(depth) => setPath(path.slice(0, depth))} />
          {here.length === 0 && <p className="mx-4 text-sm text-muted">Nothing in this folder.</p>}
          <div className="grid grid-cols-2 gap-3 px-3 sm:grid-cols-3">
            {[...here].sort((a, b) => Number(b.folder) - Number(a.folder)).map((e) => (e.folder ? <Folder key={e.id} entry={e} open={() => setPath([...path, e])} /> : <Notebook key={e.id} entry={e} on={picked.has(e.id)} toggle={() => toggle(e.id)} />))}
          </div>
        </>
      )}
      <div className="fixed inset-x-3 bottom-[calc(76px+env(safe-area-inset-bottom,0px))] z-20 mx-auto flex max-w-3xl items-center gap-2.5 rounded-[22px] border border-line bg-soft py-2.5 pr-2.5 pl-4 shadow-lift lg:right-3 lg:bottom-4 lg:left-[calc(18rem+0.75rem)]">
        <Swimmer className="bob absolute top-[-24px] left-[18px]" />
        <span className="text-[13.5px] text-ink-2">{picked.size} selected</span>
        {failure && <span role="alert" className="min-w-0 text-xs text-danger">Could not start the ingest.</span>}
        <button type="button" disabled={busy || picked.size === 0} onClick={() => void start()} className="ml-auto h-11 cursor-pointer rounded-[14px] border-0 bg-ink px-5 text-sm font-semibold text-on-ink disabled:cursor-default disabled:opacity-50">
          {picked.size ? `Ingest ${plural(picked.size, 'notebook')}` : 'Ingest'}
        </button>
      </div>
    </div>
  );
}

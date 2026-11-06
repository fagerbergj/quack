import { json } from '../../api.ts';

export type State = 'new' | 'changed' | 'ingesting' | 'failed' | 'current';
export type Entry = { id: string; name: string; folder: boolean; modified: string; state?: State; error?: string; children?: Entry[] };

// The page never holds the reMarkable service token: the distributor adds it (apps/remarkable/*).
export const documentTree = async () => (await json<{ tree: Entry[] }>(await fetch('apps/remarkable'))).tree;
export const ingest = async (ids: string[]) => json<{ batch: string }>(await fetch('apps/remarkable/ingest', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ ids }) }));

export const notebooks = (es: Entry[]): Entry[] => es.flatMap((e) => (e.folder ? notebooks(e.children ?? []) : [e]));

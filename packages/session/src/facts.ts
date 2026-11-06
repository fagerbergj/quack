// What a session's directory says about it beyond its log: read alike by the distributor's listing, the session's own
// lifecycle stream and the catalog backfill.
import { readFileSync } from 'node:fs';
import { readFile, stat, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { SessionFacts } from './lifecycle.ts';

const readText = (path: string) => readFile(path, 'utf8').catch(() => undefined);

// The session store records no wall-clock creation time, so `created` is written once with the session.
// Older sessions backfill it once from their first logged message, else the session-key file's mtime.
export async function createdAt(dir: string): Promise<number> {
  const recorded = Number(await readText(join(dir, 'created')));
  if (recorded) return recorded;
  const first = (await logTimes(dir, 'min')) || (await stat(join(dir, 'session-key')).then((s) => Math.floor(s.mtimeMs), () => 0));
  if (first) await writeFile(join(dir, 'created'), String(first)).catch(() => undefined);
  return first;
}

async function logTimes(dir: string, fn: 'min' | 'max'): Promise<number> {
  try {
    const db = new DatabaseSync(join(dir, 'session.sqlite'), { readOnly: true });
    try {
      const row = db.prepare(`SELECT ${fn}(json_extract(m.value, '$.timestamp')) AS at FROM entries, json_each(entries.record, '$.model') m`).get() as { at: number | null };
      return row.at ?? 0;
    } finally {
      db.close();
    }
  } catch {
    return 0;
  }
}

// The stored `activity` file is written when a run completes; a session without one is backfilled once from its log (and the older `touched` file), like `created`.
export async function lastActivity(dir: string): Promise<number> {
  const stored = Number(await readText(join(dir, 'activity')));
  if (stored) return stored;
  const at = Math.max(await logTimes(dir, 'max'), Number(await readText(join(dir, 'touched'))) || 0) || (await createdAt(dir));
  // `wx`: a touch that lands during the backfill keeps its value.
  if (at) await writeFile(join(dir, 'activity'), String(at), { flag: 'wx' }).catch(() => undefined);
  return at;
}

export const TITLE_MAX = 120;

// A title the caller gave on the first submission (the session's `title` file) beats the first message.
export function recordedTitle(dir: string): string | undefined {
  try {
    return readFileSync(join(dir, 'title'), 'utf8').trim() || undefined;
  } catch {
    return undefined;
  }
}

// The session's first user message, read straight from its store so listing never wakes a process.
export function firstInput(dir: string): string | undefined {
  let db: DatabaseSync | undefined;
  try {
    db = new DatabaseSync(join(dir, 'session.sqlite'), { readOnly: true });
    const row = db.prepare("SELECT json_extract(m.value, '$.content') AS c, json_type(m.value, '$.content') AS t FROM entries e, json_each(e.record, '$.model') m WHERE json_extract(m.value, '$.role') = 'user' ORDER BY e.id LIMIT 1").get() as { c?: string; t?: string } | undefined;
    const parts: unknown = row?.t === 'array' && row.c ? JSON.parse(row.c) : row?.c;
    const text = Array.isArray(parts) ? parts.map((p) => (p?.type === 'text' ? p.text : '')).join(' ') : parts;
    return typeof text === 'string' && text.trim() ? text.trim().replace(/\s+/g, ' ').slice(0, TITLE_MAX) : undefined;
  } catch {
    return undefined;
  } finally {
    db?.close();
  }
}


/** The facts a session's lifecycle stream reports, from its directory. */
export async function sessionFacts(dir: string): Promise<SessionFacts> {
  const version = JSON.parse((await readText(join(dir, 'agent-def', 'agent.json'))) ?? 'null')?.version;
  const title = recordedTitle(dir)?.slice(0, TITLE_MAX) ?? firstInput(dir);
  return { agent: (await readText(join(dir, 'agent')))?.trim() ?? '', version: typeof version === 'number' ? version : null, createdAt: await createdAt(dir), lastActivity: await lastActivity(dir), ...(title && { title }) };
}

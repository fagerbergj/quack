// What a session's directory says about it beyond its log: read alike by the session's own lifecycle stream and the
// catalog's recovery.
import { readFileSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { SessionFacts } from './lifecycle.ts';

const readText = (path: string) => readFile(path, 'utf8').catch(() => undefined);

// The session store records no wall-clock creation time, so the distributor writes `created` once with the session.
export const createdAt = async (dir: string): Promise<number> => Number(await readText(join(dir, 'created'))) || 0;

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
  return { agent: (await readText(join(dir, 'agent')))?.trim() ?? '', version: typeof version === 'number' ? version : null, createdAt: await createdAt(dir), ...(title && { title }) };
}

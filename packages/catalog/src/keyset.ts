import { createHash } from 'node:crypto';
import type { Fragment, Param, Table } from './compile.ts';

/** A cursor this store would not issue for the (where, orderBy) it came with: the REST surface's 400. */
export class CursorError extends Error {}

type Col = { expr: string; desc: boolean; numeric: boolean };
const LAST = String(Number.MAX_SAFE_INTEGER);

// Every order ends on the list's key, so a page boundary is one exact position.
const ORDERS: Record<Table, Record<string, (a: string) => Col[]>> = {
  sessions: {
    'lastActivity:newest': (a) => [{ expr: `${a}.last_activity`, desc: true, numeric: true }, { expr: `${a}.key`, desc: false, numeric: false }],
    'createdAt:newest': (a) => [{ expr: `${a}.created_at`, desc: true, numeric: true }, { expr: `${a}.key`, desc: false, numeric: false }],
    // A session with nothing pending sorts after every wait.
    'oldestAskAt:oldest': (a) => [{ expr: `coalesce(${a}.oldest_ask_at, ${LAST})`, desc: false, numeric: true }, { expr: `${a}.key`, desc: false, numeric: false }],
  },
  asks: Object.fromEntries((['newest', 'oldest'] as const).map((dir) => [`createdAt:${dir}`, (a: string) => [
    { expr: `${a}.created_at`, desc: dir === 'newest', numeric: true }, { expr: `${a}.session`, desc: false, numeric: false }, { expr: `${a}.id`, desc: false, numeric: false },
  ]])),
  artifacts: {
    'sealedAt:newest': (a) => [{ expr: `${a}.sealed_at`, desc: true, numeric: true }, { expr: `${a}.session`, desc: false, numeric: false },
      { expr: `${a}.kind`, desc: false, numeric: false }, { expr: `${a}.version`, desc: false, numeric: true }],
  },
};

const canonical = (v: unknown) => JSON.stringify(v, (_k, x) => (typeof x === 'bigint' ? `${x}n` : x));
const bindingOf = (where: unknown, orderBy: unknown) => createHash('sha256').update(canonical([where, orderBy])).digest('base64url').slice(0, 22);
const encode = (binding: string, values: Param[]) => Buffer.from(JSON.stringify([binding, values])).toString('base64url');

/** One keyset page's order, bound to its query: the ORDER BY, the seek past a cursor, and the next cursor. */
export function keyset(table: Table, alias: string, where: unknown, orderBy: { field: string; direction: string }) {
  const cols = ORDERS[table][`${orderBy.field}:${orderBy.direction}`]?.(alias);
  if (!cols) throw new Error(`catalog: ${table} has no order ${orderBy.field} ${orderBy.direction}; the core validates orders, so this is a core bug`);
  const binding = bindingOf(where, orderBy);
  return {
    select: cols.map((c, i) => `${c.expr} as k${i}`).join(', '),
    orderBy: cols.map((c) => `${c.expr} ${c.desc ? 'desc' : 'asc'}`).join(', '),
    /** The rows strictly after the cursor's position; refuses a cursor not issued for this query and order. */
    after(cursor: string | undefined): Fragment {
      if (cursor === undefined) return { sql: '1 = 1', params: [] };
      let parsed: unknown;
      try {
        parsed = JSON.parse(Buffer.from(cursor, 'base64url').toString());
      } catch {
        throw new CursorError('pageToken is not one this deployment issued');
      }
      const [bound, values] = Array.isArray(parsed) ? parsed : [];
      const fits = Array.isArray(values) && values.length === cols.length && values.every((v, i) => typeof v === (cols[i]!.numeric ? 'number' : 'string'));
      if (!fits || encode(bound, values) !== cursor) throw new CursorError('pageToken is not one this deployment issued');
      if (bound !== binding) throw new CursorError('pageToken was issued for a different q or orderBy');
      const branches = cols.map((c, i) => ({ sql: `(${[...cols.slice(0, i).map((p) => `${p.expr} = ?`), `${c.expr} ${c.desc ? '<' : '>'} ?`].join(' and ')})`, params: values.slice(0, i + 1) }));
      return { sql: `(${branches.map((b) => b.sql).join(' or ')})`, params: branches.flatMap((b) => b.params) };
    },
    /** The cursor that resumes after this row, read from its k0..kn columns. */
    next(row: Record<string, unknown>): string {
      return encode(binding, cols.map((c, i) => (c.numeric ? Number(row[`k${i}`]) : String(row[`k${i}`]))));
    },
  };
}

import type { SessionInfo } from './process-driver.ts';

type Keep = (s: SessionInfo) => boolean;
type Position = [at: number, key: string];

// field:value, with a bare value or a double-quoted one; terms join with AND (a Lucene subset an external search backend can take over).
const TERM = /(-?)([a-z]+):("(?:[^"\\]|\\.)*"|[A-Za-z0-9:/#._@*-]+)/y;
const JOIN = /\s+AND\s+/y;

function flagTerm(field: 'running' | 'archived', value: string, exact: boolean): Keep | string {
  if (!exact || (value !== 'true' && value !== 'false')) return `filter: ${field} takes true or false`;
  return (s) => Boolean(s[field]) === (value === 'true');
}

function textTerm(field: 'key' | 'agent', value: string, prefix: boolean): Keep | string {
  if (prefix && field !== 'key') return 'filter: only key takes a prefix (value*)';
  return prefix ? (s) => s.key.startsWith(value) : (s) => s[field] === value;
}

function term(negated: string, field: string, raw: string): Keep | string {
  const quoted = raw.startsWith('"');
  const value = quoted ? raw.slice(1, -1).replace(/\\(.)/g, '$1') : raw;
  const prefix = !quoted && value.endsWith('*');
  const text = prefix ? value.slice(0, -1) : value;
  if (!quoted && text.includes('*')) return 'filter: * is only allowed at the end of an unquoted value';
  const keep = field === 'running' || field === 'archived' ? flagTerm(field, text, !quoted && !prefix) : field === 'key' || field === 'agent' ? textTerm(field, text, prefix) : `filter: unsupported field ${field}`;
  return typeof keep === 'string' || !negated ? keep : (s) => !keep(s);
}

/** A predicate for a `filter` expression, or the 400 detail. Fields: key, agent, running, archived. */
export function parseFilter(text: string): Keep | string {
  const src = text.trim();
  const terms: Keep[] = [];
  for (let at = 0; src; ) {
    TERM.lastIndex = at;
    const m = TERM.exec(src);
    if (!m) return `filter: expected field:value at position ${at}`;
    const keep = term(m[1]!, m[2]!, m[3]!);
    if (typeof keep === 'string') return keep;
    terms.push(keep);
    at = TERM.lastIndex;
    if (at === src.length) break;
    JOIN.lastIndex = at;
    if (!JOIN.exec(src)) return `filter: expected AND at position ${at}`;
    at = JOIN.lastIndex;
  }
  return (s) => terms.every((keep) => keep(s));
}

export const DEFAULT_PAGE = 100;
export const MAX_PAGE = 1000;

const tokenOf = ([at, key]: Position) => Buffer.from(JSON.stringify([at, key])).toString('base64url');

// Only a token this server would have issued: it must re-encode to itself.
function positionOf(token: string): Position | undefined {
  try {
    const [at, key] = JSON.parse(Buffer.from(token, 'base64url').toString()) as unknown[];
    return Number.isSafeInteger(at) && typeof key === 'string' && tokenOf([at as number, key]) === token ? [at as number, key] : undefined;
  } catch {
    return undefined;
  }
}

export type ListQuery = { keep: Keep; size: number; after?: Position };

/** The list request's filter and page, or the 400 detail. */
export function listQuery(q: URLSearchParams): ListQuery | string {
  const keep = parseFilter(q.get('filter') ?? '');
  if (typeof keep === 'string') return keep;
  const raw = q.get('pageSize');
  if (raw !== null && !/^[1-9]\d*$/.test(raw)) return 'pageSize must be a positive integer';
  const token = q.get('pageToken');
  const after = token ? positionOf(token) : undefined;
  if (token && !after) return 'pageToken is not one this server issued';
  return { keep, size: Math.min(raw ? Number(raw) : DEFAULT_PAGE, MAX_PAGE), after };
}

const cmp = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

/** One page in (lastActivity descending, key ascending) order, resuming strictly after `after`. */
export function paged<T extends { key: string; lastActivity: string }>(all: T[], { size, after }: Pick<ListQuery, 'size' | 'after'>): { rows: T[]; next?: string } {
  const at = (s: T) => Date.parse(s.lastActivity);
  const sorted = [...all].sort((a, b) => at(b) - at(a) || cmp(a.key, b.key));
  const rest = after ? sorted.filter((s) => at(s) < after[0] || (at(s) === after[0] && cmp(s.key, after[1]) > 0)) : sorted;
  const rows = rest.slice(0, size);
  const last = rows.at(-1);
  return { rows, ...(rest.length > size && last && { next: tokenOf([at(last), last.key]) }) };
}

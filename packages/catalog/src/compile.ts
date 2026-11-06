import { unescapeTerm } from '@quack/sdk/query';

export type Table = 'sessions' | 'asks' | 'artifacts';
export type Param = string | number | bigint | null;
export type Fragment = { sql: string; params: Param[] };

/** A full-text query both adapters run the same way: lowercase alphanumeric tokens, adjacent, the last maybe a prefix. */
export type FtsQuery = { tokens: string[]; prefix: boolean };

/** What the two SQL dialects do differently when a list is compiled. */
export type QueryDialect = {
  /** A boolean false that survives coalesce in this dialect. */
  readonly FALSE: string;
  /** expr matches the anchored regular expression in the one parameter. */
  regex(expr: string): string;
  fts(table: Table, alias: string, q: FtsQuery): Fragment;
};

/** The text a collection's bare terms search: lowercase letters and digits, single-spaced, so FTS5 and tsvector agree. */
export const ftsText = (...parts: Array<string | undefined | null>) =>
  parts.filter(Boolean).join(' ').toLowerCase().replace(/[^\p{L}\p{N}]+/gu, ' ').trim();

const COLUMNS: Record<Table, Record<string, string>> = {
  sessions: {
    key: 'key', agent: 'agent', title: 'title', state: 'state', createdAt: 'created_at', lastActivity: 'last_activity', 'cost.currencyCode': 'currency',
    'cost.amountMicros': 'cost_micros', 'tokens.input': 'tokens_input', 'tokens.output': 'tokens_output', 'tokens.cacheRead': 'tokens_cache_read',
    'tokens.cacheWrite': 'tokens_cache_write', pendingAsks: 'pending_asks', oldestAskAt: 'oldest_ask_at', 'lastRun.outcome': 'last_outcome',
  },
  asks: { session: 'session', type: 'type', state: 'state', createdAt: 'created_at', resolvedAt: 'resolved_at', tool: 'tool' },
  artifacts: { session: 'session', kind: 'kind', version: 'version', createdAt: 'created_at', 'verdict.passed': 'verdict_passed' },
};

const REGEX_SPECIAL = /[\\^$.|?*+()[\]{}]/g;

/** A Lucene-escaped glob as an anchored regex: unescaped * and ? are wildcards, everything else is literal. */
export function globRegex(raw: string, any = '.', literal = (c: string) => c.replace(REGEX_SPECIAL, '\\$&')): string {
  const { text, wildcards } = unescapeTerm(raw);
  const stars = new Set(wildcards);
  return text.split('').map((c, i) => (!stars.has(i) ? literal(c) : c === '*' ? `${any}*` : any)).join('');
}

const param = (v: unknown): Param => (typeof v === 'boolean' ? (v ? 1 : 0) : (v as Param));

const OPS = { gt: '>', gte: '>=', lt: '<', le: '<=' } as const;
type Bound = { value: unknown; inclusive: boolean };
type Leaf = { kind: 'match'; field: string; value: unknown } | { kind: 'prefix'; field: string; value: string } | { kind: 'wildcard'; field: string; pattern: string }
  | { kind: 'compare'; field: string; op: keyof typeof OPS; value: unknown } | { kind: 'range'; field: string; from?: Bound; to?: Bound };
type Text = { kind: 'text'; value: string; phrase?: boolean; wildcard?: boolean };
/** Any collection's AST, as the adapters walk it; the port's QueryAst types each field's value. */
export type Ast = { kind: 'all' } | { kind: 'and' | 'or'; terms: Ast[] } | { kind: 'not'; term: Ast } | Text | Leaf;

function leaf(d: QueryDialect, col: string, n: Leaf): Fragment {
  switch (n.kind) {
    case 'match': return { sql: `${col} = ?`, params: [param(n.value)] };
    case 'prefix': return { sql: `substr(${col}, 1, ?) = ?`, params: [[...n.value].length, n.value] };
    case 'wildcard': return { sql: d.regex(col), params: [`^${globRegex(n.pattern)}$`] };
    case 'compare': return { sql: `${col} ${OPS[n.op]} ?`, params: [param(n.value)] };
    case 'range': {
      const parts = [n.from && { sql: `${col} ${n.from.inclusive ? '>=' : '>'} ?`, params: [param(n.from.value)] }, n.to && { sql: `${col} ${n.to.inclusive ? '<=' : '<'} ?`, params: [param(n.to.value)] }].filter((p) => !!p);
      return parts.length ? join(parts, 'and') : { sql: '1 = 1', params: [] };
    }
  }
}

// A bare term is its tokens side by side; a wildcard beyond one trailing * is a token-bounded regex over the text column.
function text(d: QueryDialect, table: Table, alias: string, n: Text): Fragment {
  if (n.wildcard) {
    const { wildcards, text: plain } = unescapeTerm(n.value);
    const tokens = ftsText(plain.replace(/[*?]+$/, ''));
    if (wildcards.length === 1 && wildcards[0] === plain.length - 1 && plain.endsWith('*') && tokens) return d.fts(table, alias, { tokens: tokens.split(' '), prefix: true });
    const body = globRegex(n.value, '[^ ]', (c) => (/[\p{L}\p{N}]/u.test(c) ? c.toLowerCase() : ' '));
    return { sql: d.regex(`${alias}.fts_text`), params: [`(^| )${body.replace(/ +/g, ' ').trim()}( |$)`] };
  }
  const tokens = ftsText(n.value);
  return tokens ? d.fts(table, alias, { tokens: tokens.split(' '), prefix: false }) : { sql: d.FALSE, params: [] };
}

const join = (parts: Fragment[], op: 'and' | 'or'): Fragment => ({ sql: `(${parts.map((p) => p.sql).join(` ${op} `)})`, params: parts.flatMap((p) => p.params) });

/** The WHERE fragment of one list; every leaf is coalesced to false, so NOT keeps rows whose field is absent. */
export function compileWhere(d: QueryDialect, table: Table, alias: string, n: Ast): Fragment {
  switch (n.kind) {
    case 'all': return { sql: '1 = 1', params: [] };
    case 'and': case 'or': return n.terms.length ? join(n.terms.map((t) => compileWhere(d, table, alias, t)), n.kind) : { sql: '1 = 1', params: [] };
    case 'not': {
      const inner = compileWhere(d, table, alias, n.term);
      return { sql: `not (${inner.sql})`, params: inner.params };
    }
    case 'text': return wrap(d, text(d, table, alias, n));
  }
  const col = COLUMNS[table][n.field];
  if (!col) throw new Error(`catalog: ${table} has no field ${n.field}; the core validates fields, so this is a core bug`);
  return wrap(d, leaf(d, `${alias}.${col}`, n));
}

const wrap = (d: QueryDialect, f: Fragment): Fragment => ({ sql: `coalesce((${f.sql}), ${d.FALSE})`, params: f.params });

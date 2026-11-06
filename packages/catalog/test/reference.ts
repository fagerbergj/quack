// The conformance suite's reference: the lists' documented semantics evaluated in memory over rows read one by one.
import { unescapeTerm } from '@quack/sdk';
import type { ArtifactRow, AskRow, SessionRow } from '../src/index.ts';

export type Listed = 'sessions' | 'asks' | 'artifacts';
type Row = Record<string, unknown>;
type Node = { kind: string; field?: string; value?: unknown; pattern?: string; op?: string; terms?: Node[]; term?: Node; phrase?: boolean; wildcard?: boolean; from?: { value: unknown; inclusive: boolean }; to?: { value: unknown; inclusive: boolean } };

const FIELDS: Record<Listed, Record<string, (r: Row) => unknown>> = {
  sessions: {
    key: (r) => r.key, agent: (r) => r.agent, title: (r) => r.title, state: (r) => r.state, createdAt: (r) => r.createdAt, lastActivity: (r) => r.lastActivity,
    'cost.currencyCode': (r) => (r as SessionRow).cost.currencyCode, 'cost.amountMicros': (r) => (r as SessionRow).cost.micros,
    'tokens.input': (r) => (r as SessionRow).tokens.input, 'tokens.output': (r) => (r as SessionRow).tokens.output,
    'tokens.cacheRead': (r) => (r as SessionRow).tokens.cacheRead, 'tokens.cacheWrite': (r) => (r as SessionRow).tokens.cacheWrite,
    pendingAsks: (r) => r.pendingAsks, oldestAskAt: (r) => r.oldestAskAt, 'lastRun.outcome': (r) => (r as SessionRow).lastRun?.outcome,
  },
  asks: { session: (r) => r.session, type: (r) => r.type, state: (r) => r.state, createdAt: (r) => r.createdAt, resolvedAt: (r) => r.resolvedAt, tool: (r) => (r as AskRow).toolCall?.tool },
  artifacts: { session: (r) => r.session, kind: (r) => r.kind, version: (r) => r.version, createdAt: (r) => r.createdAt, 'verdict.passed': (r) => (r as ArtifactRow).verdictPassed },
};

// The documented full-text sources, in the order the store joins them.
const SOURCES: Record<Listed, (r: Row) => unknown[]> = {
  sessions: (r) => [r.title, r.key, r.lastMessage],
  asks: (r) => [(r as AskRow).prompt?.question, (r as AskRow).toolCall?.tool],
  artifacts: (r) => [r.summary, r.outcome],
};

const words = (s: string) => s.toLowerCase().split(/[^\p{L}\p{N}]+/u).filter(Boolean);

/** Whether the glob (unescaped * and ? wild) matches the whole string. */
function glob(raw: string, s: string, any = /[\s\S]/): boolean {
  const { text, wildcards } = unescapeTerm(raw);
  const at = new Set(wildcards);
  const source = text.split('').map((c, i) => (at.has(i) ? `${any.source}${c === '*' ? '*' : ''}` : c.replace(/[\\^$.|?*+()[\]{}]/g, '\\$&'))).join('');
  return new RegExp(`^${source}$`).test(s);
}

function textMatch(list: Listed, r: Row, n: Node): boolean {
  const doc = words(SOURCES[list](r).filter((x) => typeof x === 'string').join(' '));
  const raw = String(n.value);
  const plain = n.wildcard ? unescapeTerm(raw) : { text: raw, wildcards: [] };
  const trailing = n.wildcard && plain.wildcards.length === 1 && plain.wildcards[0] === plain.text.length - 1 && plain.text.endsWith('*');
  if (n.wildcard && !trailing) {
    // Wildcards stay inside one token; each run of other non-alphanumerics is a token boundary.
    const at = new Set(plain.wildcards);
    const body = plain.text.split('').map((c, i) => (at.has(i) ? (c === '*' ? '[^ ]*' : '[^ ]') : /[\p{L}\p{N}]/u.test(c) ? c.toLowerCase() : ' ')).join('');
    return new RegExp(`(^| )${body.replace(/ +/g, ' ').trim()}( |$)`).test(doc.join(' '));
  }
  const want = words(trailing ? plain.text.slice(0, -1) : plain.text);
  if (!want.length) return false;
  return doc.some((_w, i) => want.every((w, k) => (trailing && k === want.length - 1 ? doc[i + k]?.startsWith(w) : doc[i + k] === w)));
}

function leaf(list: Listed, r: Row, n: Node): boolean {
  const v = FIELDS[list][n.field!]!(r) as string | number | bigint | boolean | undefined;
  if (v === undefined) return false;
  const want = n.value as typeof v;
  switch (n.kind) {
    case 'match': return v === want;
    case 'prefix': return String(v).startsWith(String(want));
    case 'wildcard': return glob(n.pattern!, String(v));
    case 'compare': return { gt: v > want, gte: v >= want, lt: v < want, le: v <= want }[n.op as 'gt']!;
    case 'range': return (!n.from || (n.from.inclusive ? v >= (n.from.value as typeof v) : v > (n.from.value as typeof v)))
      && (!n.to || (n.to.inclusive ? v <= (n.to.value as typeof v) : v < (n.to.value as typeof v)));
  }
  throw new Error(`reference: unknown node ${n.kind}`);
}

function matches(list: Listed, r: Row, n: Node): boolean {
  switch (n.kind) {
    case 'all': return true;
    case 'and': return n.terms!.every((t) => matches(list, r, t));
    case 'or': return n.terms!.some((t) => matches(list, r, t));
    case 'not': return !matches(list, r, n.term!);
    case 'text': return textMatch(list, r, n);
  }
  return leaf(list, r, n);
}

const cmp = (a: unknown, b: unknown) => ((a as number) < (b as number) ? -1 : (a as number) > (b as number) ? 1 : 0);
const TIEBREAK: Record<Listed, (r: Row) => unknown[]> = {
  sessions: (r) => [r.key], asks: (r) => [r.session, r.id], artifacts: (r) => [r.session, r.kind, r.version],
};

/** The rows a list returns, in its order with the list's key as final tiebreak. */
export function reference<T extends Row>(list: Listed, rows: T[], where: unknown, order: { field: string; direction: string }): T[] {
  const sortValue = (r: Row) => (order.field === 'oldestAskAt' ? (r.oldestAskAt ?? Number.MAX_SAFE_INTEGER) : r[order.field]);
  const sign = order.direction === 'newest' ? -1 : 1;
  return rows.filter((r) => matches(list, r, where as Node)).sort((a, b) => sign * cmp(sortValue(a), sortValue(b))
    || TIEBREAK[list](a).map((x, i) => cmp(x, TIEBREAK[list](b)[i])).find((c) => c !== 0) || 0);
}

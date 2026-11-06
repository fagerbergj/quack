import { parse, type LiqeQuery } from 'liqe';
import { unescapeTerm } from '@quack/sdk/query';
import type { ArtifactField, AskField, QueryAst, SessionField } from './port.ts';

/** A q the list cannot run: the HTTP adapter answers it 400 with this message as the detail. */
export class QueryError extends Error {}

type ValueKind = 'text' | 'string' | 'int' | 'micros' | 'date' | 'bool' | readonly string[];

// RFC 3339: a date-time carries seconds and Z or an offset, so a q means the same rows on every host; a date is UTC midnight.
const DATE = /^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2}))?$/;
const ZONELESS = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?$/;

// Each list's q field set: 'text' fields also take prefix and wildcard terms.
const SESSIONS: Record<SessionField, ValueKind> = {
  key: 'text', agent: 'text', title: 'text', state: ['running', 'idle', 'stopped', 'archived'], createdAt: 'date', lastActivity: 'date',
  'cost.currencyCode': 'string', 'cost.amountMicros': 'micros', 'tokens.input': 'int', 'tokens.output': 'int', 'tokens.cacheRead': 'int', 'tokens.cacheWrite': 'int',
  pendingAsks: 'int', oldestAskAt: 'date', 'lastRun.outcome': ['succeeded', 'failed', 'aborted'],
};
const ASKS: Record<AskField, ValueKind> = { session: 'text', type: ['approval', 'question'], state: ['pending', 'resolved', 'cancelled'], createdAt: 'date', resolvedAt: 'date', tool: 'text' };
const ARTIFACTS: Record<ArtifactField, ValueKind> = { session: 'text', kind: 'text', version: 'int', createdAt: 'date', 'verdict.passed': 'bool' };
const { session: _askSession, ...SESSION_ASKS } = ASKS;
const { session: _artifactSession, ...SESSION_ARTIFACTS } = ARTIFACTS;

/** The lists q is parsed for; the per-session lists lack `session`, which the core folds in. */
export const LISTS = { sessions: SESSIONS, asks: ASKS, sessionAsks: SESSION_ASKS, artifacts: ARTIFACTS, sessionArtifacts: SESSION_ARTIFACTS } as const;
export type ListName = keyof typeof LISTS;
type FieldOf<L extends ListName> = L extends 'sessions' ? SessionField : L extends 'asks' | 'sessionAsks' ? AskField : ArtifactField;

type Value = string | number | bigint | boolean;
type Leaf = Record<string, unknown> & { kind: string };
const OPS: Record<string, 'gt' | 'gte' | 'lt' | 'le'> = { ':>': 'gt', ':>=': 'gte', ':<': 'lt', ':<=': 'le' };

function typed(field: string, kind: ValueKind, text: string): Value {
  const bad = (what: string) => new QueryError(`${field} takes ${what}, not ${JSON.stringify(text)}`);
  if (Array.isArray(kind)) {
    if (!kind.includes(text)) throw bad(`one of ${kind.join(', ')}`);
    return text;
  }
  if (kind === 'int') {
    if (!/^\d+$/.test(text) || !Number.isSafeInteger(Number(text))) throw bad('a non-negative integer');
    return Number(text);
  }
  if (kind === 'micros') {
    if (!/^\d+$/.test(text) || BigInt(text) >= 2n ** 63n) throw bad('an integer count of micros');
    return BigInt(text);
  }
  if (kind === 'date') {
    if (ZONELESS.test(text)) throw new QueryError(`${field} takes a date-time with Z or an offset, so it reads the same on every host: add one, as in ${text.length === 16 ? `${text}:00` : text}Z`);
    const ms = DATE.test(text) ? Date.parse(text) : NaN;
    if (Number.isNaN(ms)) throw bad('an RFC 3339 date or date-time');
    return ms;
  }
  if (kind === 'bool') {
    if (text !== 'true' && text !== 'false') throw bad('true or false');
    return text === 'true';
  }
  return text;
}

// An unquoted term keeps its source text, so a number past 2^53 and a backslash escape both survive liqe's own reading.
function rawOf(q: string, node: { location: { start: number; end: number }; quoted?: boolean; value?: unknown }) {
  return node.quoted ? { quoted: true, raw: String(node.value) } : { quoted: false, raw: q.slice(node.location.start, node.location.end) };
}

function rangeLeaf(q: string, field: string, kind: ValueKind, tag: LiqeQuery & { type: 'Tag' }): Leaf {
  if (tag.expression.type !== 'RangeExpression') throw new QueryError('not a range');
  const { range, location } = tag.expression;
  // liqe reads range bounds as floats; numeric bounds are re-read from the source text.
  const source = /^[[{]\s*(\S+)\s+TO\s+(\S+)\s*[\]}]$/.exec(q.slice(location.start, location.end));
  if (!source) throw new QueryError(`${field}: cannot re-read range bounds from the source`);
  const bound = (v: number | string, rawText: string) => typed(field, kind, typeof v === 'number' ? rawText : v);
  return { kind: 'range', field, from: { value: bound(range.min, source[1]!), inclusive: range.minInclusive }, to: { value: bound(range.max, source[2]!), inclusive: range.maxInclusive } };
}

function termLeaf(field: string, kind: ValueKind, quoted: boolean, raw: string): Leaf {
  const { text, wildcards } = quoted ? { text: raw, wildcards: [] } : unescapeTerm(raw);
  if (!wildcards.length) return { kind: 'match', field, value: typed(field, kind, text) };
  if (kind !== 'text') throw new QueryError(`${field} takes no wildcard; * and ? apply to text fields and bare terms only`);
  const prefix = wildcards.length === 1 && wildcards[0] === text.length - 1 && text.endsWith('*');
  return prefix ? { kind: 'prefix', field, value: text.slice(0, -1) } : { kind: 'wildcard', field, pattern: raw };
}

function bareTerm(q: string, tag: LiqeQuery & { type: 'Tag' }): Leaf {
  const expr = tag.expression;
  if (expr.type !== 'LiteralExpression') throw new QueryError('a bare term is a word or a quoted phrase');
  const { quoted, raw } = rawOf(q, expr);
  if (quoted) return { kind: 'text', value: raw, phrase: true };
  const { text, wildcards } = unescapeTerm(raw);
  return wildcards.length ? { kind: 'text', value: raw, wildcard: true } : { kind: 'text', value: text };
}

function tagLeaf(q: string, fields: Record<string, ValueKind>, tag: LiqeQuery & { type: 'Tag' }): Leaf {
  if (tag.field.type === 'ImplicitField') return bareTerm(q, tag);
  const field = tag.field.name;
  const kind = fields[field];
  if (tag.field.quoted || kind === undefined) throw new QueryError(`unknown field ${JSON.stringify(field)}; this list's fields are ${Object.keys(fields).join(', ')}`);
  const expr = tag.expression;
  if (expr.type === 'RegexExpression') throw new QueryError('regular expressions are not supported; use * and ? wildcards');
  if (expr.type === 'EmptyExpression') throw new QueryError(`${field} has no value`);
  if (expr.type === 'RangeExpression') return rangeLeaf(q, field, kind, tag);
  const { quoted, raw } = rawOf(q, expr);
  const op = OPS[tag.operator.operator];
  return op ? { kind: 'compare', field, op, value: typed(field, kind, quoted ? raw : unescapeTerm(raw).text) } : termLeaf(field, kind, quoted, raw);
}

function convert(q: string, fields: Record<string, ValueKind>, node: LiqeQuery): Leaf {
  switch (node.type) {
    case 'EmptyExpression': return { kind: 'all' };
    case 'ParenthesizedExpression': return convert(q, fields, node.expression);
    case 'UnaryOperator': return { kind: 'not', term: convert(q, fields, node.operand) };
    case 'Tag': return tagLeaf(q, fields, node);
    case 'LogicalExpression': {
      const kind = node.operator.operator === 'OR' ? 'or' : 'and';
      const flat = (n: Leaf): Leaf[] => (n.kind === kind ? (n.terms as Leaf[]) : [n]);
      return { kind, terms: [...flat(convert(q, fields, node.left)), ...flat(convert(q, fields, node.right))] };
    }
  }
}

/** Parses one list's q with liqe into the port's AST, checked against that list's field set; throws QueryError. */
export function parseQuery<L extends ListName>(list: L, q: string | null | undefined): QueryAst<FieldOf<L>> {
  if (!q?.trim()) return { kind: 'all' };
  let ast: LiqeQuery;
  try {
    ast = parse(q);
  } catch (err) {
    throw new QueryError(`${err instanceof Error ? err.message : String(err)}: escape a reserved character in a value with a backslash (key:a\\:b) or quote the value (key:"a:b")`);
  }
  return convert(q, LISTS[list], ast) as QueryAst<FieldOf<L>>;
}

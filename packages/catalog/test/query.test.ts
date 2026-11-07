import assert from 'node:assert/strict';
import { test } from 'node:test';
import { escapeTerm } from '@quack/sdk';
import { parseQuery, QueryError } from '../src/query.ts';

const q = String.raw;

test('an absent or empty q selects every row', () => {
  assert.deepEqual(parseQuery('sessions', undefined), { kind: 'all' });
  assert.deepEqual(parseQuery('sessions', '  '), { kind: 'all' });
});

test('escaped colons and slashes stay in exact, prefix and wildcard leaves', () => {
  assert.deepEqual(parseQuery('asks', q`session:review\:monorepo`), { kind: 'match', field: 'session', value: 'review:monorepo' });
  assert.deepEqual(parseQuery('sessions', q`key:sleeper\:platinum\:*`), { kind: 'prefix', field: 'key', value: 'sleeper:platinum:' });
  assert.deepEqual(parseQuery('sessions', q`key:a\/b*c?`), { kind: 'wildcard', field: 'key', pattern: q`a\/b*c?` });
  assert.deepEqual(parseQuery('sessions', q`key:a\*b`), { kind: 'match', field: 'key', value: 'a*b' }, 'an escaped star is a literal');
  assert.deepEqual(parseQuery('asks', 'session:"review:monorepo*"'), { kind: 'match', field: 'session', value: 'review:monorepo*' }, 'a quoted value is an exact literal');
});

test('an unescaped colon in a value is a 400 that says to escape or quote', () => {
  assert.throws(() => parseQuery('sessions', 'key:a:b'), (err: Error) => err instanceof QueryError && /escape/.test(err.message) && /quote/.test(err.message));
});

test('escapeTerm round-trips every key through the parser', () => {
  for (const key of ['sleeper:platinum:2:lineup', 'a/b#c@d.e-f_g', 'AND', '2:x', 'with space', 'café:ünï', '.dot', '-lead', 'q"uote\\back']) {
    assert.deepEqual(parseQuery('sessions', `key:${escapeTerm(key)}`), { kind: 'match', field: 'key', value: key }, key);
    assert.deepEqual(parseQuery('sessions', `key:${escapeTerm(key)}*`), { kind: 'prefix', field: 'key', value: key }, key);
  }
});

test('operators, groups, negation and implicit AND', () => {
  assert.deepEqual(parseQuery('sessions', q`key:sleeper\:* AND -state:archived`), {
    kind: 'and', terms: [{ kind: 'prefix', field: 'key', value: 'sleeper:' }, { kind: 'not', term: { kind: 'match', field: 'state', value: 'archived' } }],
  });
  assert.deepEqual(parseQuery('sessions', '(state:running OR state:idle) pendingAsks:>0 NOT lastRun.outcome:failed'), {
    kind: 'and', terms: [
      { kind: 'or', terms: [{ kind: 'match', field: 'state', value: 'running' }, { kind: 'match', field: 'state', value: 'idle' }] },
      { kind: 'compare', field: 'pendingAsks', op: 'gt', value: 0 },
      { kind: 'not', term: { kind: 'match', field: 'lastRun.outcome', value: 'failed' } },
    ],
  });
});

test('values are typed by their field: micros past 2^53 stay exact, dates become epoch ms', () => {
  assert.deepEqual(parseQuery('sessions', 'cost.amountMicros:>=9007199254740993'), { kind: 'compare', field: 'cost.amountMicros', op: 'gte', value: 9007199254740993n });
  assert.deepEqual(parseQuery('sessions', 'cost.amountMicros:[1 TO 9007199254740993}'), { kind: 'range', field: 'cost.amountMicros', from: { value: 1n, inclusive: true }, to: { value: 9007199254740993n, inclusive: false } });
  assert.deepEqual(parseQuery('sessions', q`createdAt:>2026-11-06T09\:00\:00Z`), { kind: 'compare', field: 'createdAt', op: 'gt', value: Date.parse('2026-11-06T09:00:00Z') });
  assert.deepEqual(parseQuery('asks', 'createdAt:["2026-11-01" TO "2026-11-06T00:00:00Z"]'), { kind: 'range', field: 'createdAt', from: { value: Date.parse('2026-11-01'), inclusive: true }, to: { value: Date.parse('2026-11-06T00:00:00Z'), inclusive: true } });
  assert.deepEqual(parseQuery('artifacts', 'sealedAt:>="2026-11-01T00:00:00Z" AND version:3'), { kind: 'and', terms: [{ kind: 'compare', field: 'sealedAt', op: 'gte', value: Date.parse('2026-11-01T00:00:00Z') }, { kind: 'match', field: 'version', value: 3 }] });
  assert.deepEqual(parseQuery('sessions', 'lastRun.outcome:denied'), { kind: 'match', field: 'lastRun.outcome', value: 'denied' });
});

test('bare terms and phrases are full text, with wildcards marked', () => {
  assert.deepEqual(parseQuery('sessions', 'lineup "match day" line*'), {
    kind: 'and', terms: [{ kind: 'text', value: 'lineup' }, { kind: 'text', value: 'match day', phrase: true }, { kind: 'text', value: 'line*', wildcard: true }],
  });
});

test('a field outside the list, a bad value, a regex or a wildcard on a non-text field is refused', () => {
  for (const [list, bad] of [['sessions', 'session:x'], ['sessionAsks', 'session:x'], ['sessions', 'state:waiting'], ['sessions', 'pendingAsks:many'], ['sessions', 'createdAt:yesterday'],
    ['sessions', 'key:/^sleeper/'], ['sessions', 'state:run*'], ['artifacts', 'verdict.passed:true'], ['artifacts', 'createdAt:>"2026-11-01"'], ['artifacts', 'outcome:approve'], ['sessions', 'title:'], ['asks', 'type:approval OR']] as const) {
    assert.throws(() => parseQuery(list, bad), QueryError, `${list} ${bad}`);
  }
});

test('a date-time without Z or an offset is refused, so no q depends on the host time zone', () => {
  for (const bare of [String.raw`createdAt:>2026-11-01T09\:00\:00`, 'createdAt:>"2026-11-01T09:00"', 'createdAt:["2026-11-01T09:00:00" TO "2026-11-02T00:00:00Z"]']) {
    assert.throws(() => parseQuery('sessions', bare), (err: Error) => err instanceof QueryError && /Z or an offset/.test(err.message), bare);
  }
  assert.throws(() => parseQuery('sessions', 'createdAt:>"2026-11-01T09:00Z"'), QueryError, 'an RFC 3339 date-time carries seconds');
});

test('dates and date-times read the same rows under every host time zone', () => {
  // A zone-less date-time is in the list: refused everywhere now, where it used to resolve in each host's zone.
  const queries = ['createdAt:2026-11-01', 'createdAt:>"2026-11-01T09:00:00Z"', 'lastActivity:<"2026-11-01T09:00:00.250+09:00"', 'oldestAskAt:["2026-11-01" TO "2026-11-02T00:00:00-05:00"}', 'createdAt:>"2026-11-01T09:00:00"'];
  const read = (q: string) => { try { return parseQuery('sessions', q); } catch (err) { return `refused: ${(err as Error).message}`; } };
  const saved = process.env.TZ;
  try {
    const seen = ['UTC', 'America/New_York', 'Asia/Tokyo'].map((tz) => {
      process.env.TZ = tz;
      return JSON.stringify(queries.map(read));
    });
    assert.equal(new Set(seen).size, 1, seen.join('\n'));
    assert.deepEqual(parseQuery('sessions', 'createdAt:2026-11-01'), { kind: 'match', field: 'createdAt', value: Date.UTC(2026, 10, 1) }, 'a date is UTC midnight');
  } finally {
    if (saved === undefined) delete process.env.TZ;
    else process.env.TZ = saved;
  }
});

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { anchoredText, anchorOf, canonical, typescriptText } from '../src/anchors.ts';
import type { ContractSpec } from '../src/index.ts';

const DTS = `import type { X } from './x';

/** Options for a judge. */
export interface Options {
  /** The rubric. */
  rubric: string;
  nested: { depth: number; items: Array<{ no: string }>; list: { leaf: boolean }[] };
  run(a: string): void;
  run(a: number): void;
  (input: string): number;
  [key: string]: unknown;
}
export interface Options {
  extra?: boolean;
}
export type Pair = { left: string } & X & { right: number };
/** Adds. */
export function add(a: number, b: { c: string }): number;
export function add(a: string): string;
export enum Mode { Fast = 1, Slow }
export const LIMIT: { max: number } = { max: 1 };
interface Hidden { secret: string }
export class Thing { field: string }
`;
const span = (s: string) => DTS.slice(DTS.indexOf(s), DTS.indexOf(s) + s.length);

test('a typescript pointer is the source span of its exported declaration, JSDoc included, with merged declarations joined in source order', () => {
  const first = DTS.slice(DTS.indexOf('/** Options for a judge'), DTS.indexOf('\nexport interface Options {\n  extra'));
  assert.equal(typescriptText(DTS, '/Options'), `${first}\n${span('export interface Options {\n  extra?: boolean;\n}')}`);
  assert.equal(typescriptText(DTS, '/add'), `${span('/** Adds. */\nexport function add(a: number, b: { c: string }): number;')}\n${span('export function add(a: string): string;')}`);
  assert.equal(typescriptText(DTS, '/LIMIT'), span('export const LIMIT: { max: number } = { max: 1 };'));
  assert.equal(typescriptText(DTS, '/Hidden'), undefined, 'not exported');
});

test('members resolve in interface bodies, type literals, intersections, deeper annotations and array elements; overloads join', () => {
  assert.equal(typescriptText(DTS, '/Options/rubric'), '/** The rubric. */\n  rubric: string;');
  assert.equal(typescriptText(DTS, '/Options/run'), 'run(a: string): void;\nrun(a: number): void;');
  assert.equal(typescriptText(DTS, '/Options/(call)'), '(input: string): number;');
  assert.equal(typescriptText(DTS, '/Options/[key: string]'), '[key: string]: unknown;');
  assert.equal(typescriptText(DTS, '/Options/extra'), 'extra?: boolean;', 'a member of the merged declaration');
  assert.equal(typescriptText(DTS, '/Options/nested/depth'), 'depth: number;');
  assert.equal(typescriptText(DTS, '/Options/nested/list/leaf'), 'leaf: boolean');
  assert.equal(typescriptText(DTS, '/Options/nested/items/no'), undefined, 'Array<T> is not an array type node');
  assert.equal(typescriptText(DTS, '/Pair/left'), 'left: string');
  assert.equal(typescriptText(DTS, '/Pair/right'), 'right: number');
  assert.equal(typescriptText(DTS, '/add/a'), 'a: number\na: string', 'a parameter of each overload');
  assert.equal(typescriptText(DTS, '/add/b/c'), 'c: string');
  assert.equal(typescriptText(DTS, '/Mode/Slow'), 'Slow');
  for (const p of ['/Thing/field', '/Options/missing', '/LIMIT/max', 'Options', '/', '/Pair/0']) assert.equal(typescriptText(DTS, p), undefined, p);
});

const spec = {
  document: { paths: { '/x': { get: { summary: 'X', tags: ['b', 'a'] } } }, list: [1, 2] },
  intent: { purpose: 'p', audience: 'a', consumers: [], guarantees: ['Plain fact', { id: 'g.one', text: 'An id fact' }], nonGoals: [{ id: 'ng', text: 'Not this' }] },
  choices: [{ id: 'c1', pointer: '', decision: 'd', why: [] }, { id: 'c2', pointer: '/x', decision: 'e', why: [] }],
  changes: [{ choice: 'c1', was: 'a', now: 'b', why: 'w' }, { choice: 'c2', was: 'c', now: 'd', why: 'w' }, { choice: 'c1', was: 'e', now: 'f', why: 'w' }],
} as unknown as ContractSpec;

test('a JSON pointer is the value there as canonical JSON; choices and change rows are canonical too; an entry is its text', () => {
  assert.equal(anchoredText(spec, { on: 'document', ptr: '/paths/~1x/get' }), '{"summary":"X","tags":["b","a"]}');
  assert.equal(anchoredText(spec, { on: 'document', ptr: '/list/1' }), '2');
  for (const ptr of ['/list/01', '/list/2', '/paths/x', 'paths']) assert.equal(anchoredText(spec, { on: 'document', ptr }), undefined, ptr);
  assert.equal(anchoredText(spec, { on: 'choice', id: 'c2' }), canonical({ decision: 'e', id: 'c2', pointer: '/x', why: [] }));
  assert.equal(anchoredText(spec, { on: 'change', choice: 'c1' }), canonical([spec.changes![0], spec.changes![2]]));
  assert.equal(anchoredText(spec, { on: 'change', choice: 'none' }), undefined);
  assert.equal(anchoredText(spec, { on: 'guarantee', text: 'Plain fact' }), 'Plain fact');
  assert.equal(anchoredText(spec, { on: 'guarantee', id: 'g.one', text: 'old words' }), 'An id fact', 'by id, the text there now');
  assert.equal(anchoredText(spec, { on: 'nonGoal', text: 'Plain fact' }), undefined, 'the other list');
  assert.equal(anchoredText({ ...spec, document: DTS }, { on: 'document', ptr: '/Mode/Fast' }), 'Fast = 1');
});

test('an anchor keeps only its own fields and every string is bounded', () => {
  assert.deepEqual(anchorOf({ on: 'choice', id: 'c1', extra: 1 }), { on: 'choice', id: 'c1' });
  assert.deepEqual(anchorOf({ on: 'guarantee', text: 't', id: 'g' }), { on: 'guarantee', text: 't', id: 'g' });
  for (const bad of [null, { on: 'line' }, { on: 'document', ptr: 'x'.repeat(1001) }, { on: 'choice', id: 'x'.repeat(201) }, { on: 'change' }, { on: 'nonGoal', text: '' }, { on: 'guarantee', text: 't', id: 'x'.repeat(201) }]) {
    assert.equal(typeof anchorOf(bad), 'string', JSON.stringify(bad)?.slice(0, 40));
  }
});

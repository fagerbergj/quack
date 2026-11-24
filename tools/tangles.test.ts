import assert from 'node:assert/strict';
import { test } from 'node:test';
import { tangleFindings } from './tangles.ts';

const P = 'packages/p/src';
const src = (files: Record<string, string>) => new Map(Object.entries(files).map(([f, text]) => [`${P}/${f}`, text]));
const gate = (base: Record<string, string>, head: Record<string, string>) => tangleFindings({ sources: src(base), moved: new Map() }, src(head));
// a.ts and b.ts reach each other through the barrel, so no import cycle holds them.
const BARREL = { 'index.ts': "export { a } from './a.ts';\nexport { b } from './b.ts';\n", 'a.ts': "import { b } from './index.ts';\nexport const a = () => b();\n", 'b.ts': 'export const b = () => 1;\n' };
const TANGLE = { ...BARREL, 'b.ts': "import { a } from './index.ts';\nexport const b = () => a();\n" };

test('a new tangle through a barrel fails at the use that closes it', () => {
  const found = gate(BARREL, TANGLE);
  assert.deepEqual(found.map((f) => [f.file, f.line]), [[`${P}/b.ts`, 2]]);
  assert.match(found[0].message, new RegExp(`tangle of 2 things across 2 files: ${P}/b.ts:2 b → a, ${P}/a.ts:2 a → b$`));
});

test('a type-only use passes', () => {
  assert.deepEqual(gate(BARREL, { ...BARREL, 'b.ts': "import { a } from './index.ts';\nexport const b = (f: typeof a): number => 1;\n" }), []);
  assert.deepEqual(gate(BARREL, { ...BARREL, 'b.ts': "import type { a } from './index.ts';\nexport const b = () => 1 as unknown as ReturnType<typeof a>;\n" }), []);
});

test('a parameter or inner const that shadows an imported thing is not a use of it', () => {
  assert.deepEqual(gate(BARREL, { ...BARREL, 'b.ts': "import { a } from './index.ts';\nexport const b = (a: () => number) => a();\n" }), []);
  assert.deepEqual(gate(BARREL, { ...BARREL, 'b.ts': "import { a } from './index.ts';\nexport function b() {\n  const a = () => 1;\n  return a();\n}\n" }), []);
});

test('an existing tangle the diff leaves alone passes', () => {
  assert.deepEqual(gate(TANGLE, { ...TANGLE, 'c.ts': "import { a } from './index.ts';\nexport const c = () => a();\n" }), []);
});

test('a tangle that an import cycle already holds is left to the cycle rule', () => {
  const direct = { 'a.ts': "import { b } from './b.ts';\nexport const a = () => b();\n", 'b.ts': "import { a } from './a.ts';\nexport const b = () => a();\n" };
  assert.deepEqual(gate({}, direct), []);
});

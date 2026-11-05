import assert from 'node:assert/strict';
import { test } from 'node:test';
import { guideAnchors, kinds, lintTypeScript, typeScriptBreaks, typescriptKind, validateSpec, type ContractSpec } from '../src/index.ts';

const SURFACE = `/** Provided by each extension, read by core at boot. */
export interface Manifest {
  /** The extension's name. */
  name: string;
}
/** Core hands this to every extension. */
export declare function start(m: Manifest): { ok: boolean };
`;
const codes = async (src: string) => (await lintTypeScript(src)).map((f) => f.code);
const breaks = async (a: string, b: string) => (await typeScriptBreaks(a, b)).map((x) => `${x.id} ${x.path}: ${x.text}`);

test('the kind is registered, keeps a .d.ts as is and ships a guide', () => {
  assert.equal(kinds.typescript, typescriptKind);
  assert.equal(typescriptKind.extract(SURFACE), SURFACE);
  assert.deepEqual(typescriptKind.renderHints, { layout: 'typescript', export: { format: 'typescript', filename: 'contract.d.ts' } });
  for (const a of ['additions', 'unions', 'tsdoc']) assert.ok(guideAnchors(typescriptKind.skill).has(a), a);
});

test('a documented surface that compiles has no findings, and may name a pi-durable type', async () => {
  assert.deepEqual(await lintTypeScript(SURFACE), []);
  const imp = `import type { Extension } from '@earendil-works/pi-durable';\n/** Core provides it; extensions return it. */\nexport type Ext = Extension;\n`;
  assert.deepEqual(await lintTypeScript(imp), []);
});

test('check reports one finding each for a compile error, a missing TSDoc, any, a default export and a foreign import', async () => {
  assert.deepEqual(await codes('/** A. */\nexport type A = Missing;\n'), ['ts2304']);
  assert.deepEqual(await codes('export type B = string;\n'), ['missing-tsdoc']);
  assert.deepEqual(await codes('/** C. */\nexport declare function c(x: any): void;\n'), ['no-any']);
  assert.deepEqual(await codes('/** D. */\ndeclare const d: string;\nexport default d;\n'), ['default-export']);
  assert.deepEqual(await codes("import type { X } from 'left-pad';\n/** E. */\nexport type E = string;\n"), ['ts2307', 'import-not-allowed']);
});

test('diff reports a removed export', async () => {
  const next = SURFACE.replace(/\/\*\* Core hands[\s\S]*$/, '');
  assert.deepEqual(await breaks(SURFACE, next), ['export-removed /start: export "start" is removed, so code that names it no longer compiles']);
});

test('diff reports a new required field as values extensions provide no longer fitting', async () => {
  const next = SURFACE.replace('name: string;', 'name: string;\n  version: string;');
  const out = await breaks(SURFACE, next);
  assert.equal(out.length, 2);
  assert.match(out[0], /^provided-breaks \/Manifest: export "Manifest": values extensions already provide no longer fit: Property 'version' is missing in type 'prev.Manifest' but required in type 'next.Manifest'/);
  // A parameter flips the direction: old callers hand over a Manifest the new start rejects.
  assert.match(out[1], /^produced-breaks \/start: .*Property .version. is missing in type .prev.Manifest./);
});

test('diff reports a narrowed return type against the old provider and a widened one against old consumers', async () => {
  const wide = '/** Core provides. */\nexport declare function f(): string | number;\n';
  const narrow = '/** Core provides. */\nexport declare function f(): string;\n';
  assert.deepEqual(await breaks(wide, narrow), ["provided-breaks /f: export \"f\": values extensions already provide no longer fit: Type '() => string | number' is not assignable to type '() => string'. Type 'string | number' is not assignable to type 'string'. Type 'number' is not assignable to type 'string'."]);
  assert.deepEqual(await breaks(narrow, wide), ["produced-breaks /f: export \"f\": values core now produces no longer fit old consumers: Type '() => string | number' is not assignable to type '() => string'. Type 'string | number' is not assignable to type 'string'. Type 'number' is not assignable to type 'string'."]);
});

test('diff finds no break in a new optional field or a new export', async () => {
  const optional = SURFACE.replace('name: string;', 'name: string;\n  /** Since 2. */\n  version?: string;');
  assert.deepEqual(await breaks(SURFACE, optional), []);
  assert.deepEqual(await breaks(SURFACE, `${SURFACE}/** New. */\nexport declare const extra: number;\n`), []);
});

test('a typescript spec validates, its pointers name exports, and a string document is required', () => {
  const spec: ContractSpec = {
    kind: 'typescript', title: 'SDK surface', version: 1,
    intent: { purpose: 'p', nonGoals: [], audience: 'a', consumers: ['extensions'], guarantees: [] },
    document: SURFACE,
    choices: [{ id: 'm', pointer: '/Manifest', decision: 'd', why: [{ source: 'guide', text: 't', ref: 'typescript#additions' }] }],
  };
  assert.deepEqual(validateSpec(JSON.stringify(spec)), []);
  assert.deepEqual(validateSpec(JSON.stringify({ ...spec, document: { type: 'object' } })).length, 1);
});

import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { defineExtension } from '@earendil-works/pi-durable';
import { piPackage } from '@quack/sdk';
import { openSession } from '@quack/session';
import { blastRadius, changedLines, touchedSymbols } from '../src/blast.ts';
import { checkoutTool } from '../src/index.ts';
import { cleanup, fakeService, remotes, tmp } from './fixtures.ts';

after(cleanup);

test('changed lines come from -U0 hunks in the new version', () => {
  const diff = ['diff --git a/x.ts b/x.ts', '--- a/x.ts', '+++ b/x.ts', '@@ -3 +3 @@', '-a', '+b', '@@ -10,0 +11,2 @@', '+c', '+d', '+++ b/y.ts', '@@ -5,2 +4,0 @@'].join('\n');
  assert.deepEqual(changedLines(diff), [{ path: 'x.ts', lines: [[3, 3], [11, 12]] }, { path: 'y.ts', lines: [[4, 4]] }]);
});

test('the innermost named symbol a change touches is the one measured', () => {
  const r = (a: number, b: number) => ({ start: { line: a, character: 0 }, end: { line: b, character: 0 } });
  const symbols = [{ name: 'Cache', kind: 5, range: r(0, 20), selectionRange: r(0, 0), children: [{ name: 'get', kind: 6, range: r(2, 5), selectionRange: r(2, 2) }, { name: 'set', kind: 6, range: r(7, 9), selectionRange: r(7, 7) }] }];
  assert.deepEqual(touchedSymbols(symbols, [[4, 4]]).map((s) => s.name), ['get']);
  assert.deepEqual(touchedSymbols(symbols, [[12, 12]]).map((s) => s.name), ['Cache'], 'a change between members falls to the class');
  assert.deepEqual(touchedSymbols(symbols, [[30, 30]]), []);
});

test('a references call that errors reports "unavailable", never "no references outside itself"', async () => {
  const r = (a: number, b: number) => ({ start: { line: a, character: 0 }, end: { line: b, character: 0 } });
  const symbols = [{ name: 'add', kind: 12, range: r(0, 2), selectionRange: r(0, 0) }];
  const run = async (references: object) => {
    const api = {
      agent: async () => ({ callable: [{ name: 'lsp_symbols' }, { name: 'lsp_references' }] }),
      env: { cwd: '/w', readTextFile: async () => ({ ok: true, value: 'function add() {}\n' }) },
      executeTool: async (tool: string) => (tool === 'lsp_symbols' ? { structuredOutput: symbols } : references),
    };
    return blastRadius(api as never, 'repo', '+++ b/x.ts\n@@ -1 +1 @@\n', {} as never);
  };
  assert.match(await run({ isError: true, diagnostics: [{ severity: 'error', message: 'ts: request timed out' }] }), /add: references unavailable: ts: request timed out$/);
  assert.match(await run({ structuredOutput: 'No matching LSP server for repo/x.ts' }), /references unavailable: No matching LSP server/);
  assert.match(await run({ structuredOutput: 'No result' }), /no references outside itself/);
});

test('a wide PR is capped with a stated note, and the calls run in parallel but in order', async () => {
  const r = (a: number) => ({ start: { line: a, character: 0 }, end: { line: a, character: 0 } });
  let inFlight = 0;
  let peak = 0;
  const api = {
    agent: async () => ({ callable: [{ name: 'lsp_symbols' }, { name: 'lsp_references' }] }),
    env: { cwd: '/w', readTextFile: async () => ({ ok: true, value: 'function f() {}\n' }) },
    executeTool: async (tool: string) => {
      peak = Math.max(peak, ++inFlight);
      await new Promise((done) => setImmediate(done));
      inFlight--;
      return tool === 'lsp_symbols' ? { structuredOutput: [{ name: 'f', kind: 12, range: r(0), selectionRange: r(0) }] } : { structuredOutput: [] };
    },
  };
  const diff = Array.from({ length: 25 }, (_, i) => `+++ b/f${i}.ts\n@@ -1 +1 @@`).join('\n');
  const out = await blastRadius(api as never, 'repo', diff, {} as never);
  assert.match(out, /Blast radius covers 20 of 25 changed files; the rest were not checked\.$/);
  assert.equal(out.split('\n').filter((l) => l.startsWith('- ')).length, 20);
  assert.ok(peak > 1 && peak <= 4, `peak ${peak}`);
  assert.doesNotMatch(await blastRadius(api as never, 'repo', diff.split('\n').slice(0, 6).join('\n'), {} as never), /covers/, 'no note when nothing was capped');
});

const base = {
  'package.json': '{ "name": "widgets", "type": "module" }\n',
  'tsconfig.json': '{ "compilerOptions": { "strict": true, "module": "nodenext" }, "include": ["src", "test"] }\n',
  'src/math.ts': 'export function add(a: number, b: number) {\n  return a + b;\n}\n',
  'src/app.ts': "import { add } from './math.ts';\n\nexport const total = add(1, 2);\n",
  'test/math.test.ts': "import { add } from '../src/math.ts';\n\nif (add(2, 2) !== 4) throw new Error('add');\n",
};

test('checkout_pr reports who calls each changed symbol, from a real language server', async () => {
  // The callers are touched by the PR too: the server answers references from the files it has open, which a checkout opens for the PR's own files.
  const { base: gitBase, head } = await remotes(base, {
    'src/math.ts': 'export function add(a: number, b: number) {\n  return b + a;\n}\n',
    'src/app.ts': `${base['src/app.ts']}// touched\n`,
    'test/math.test.ts': `${base['test/math.test.ts']}// touched\n`,
  });
  const service = await fakeService(head);
  const agentDir = await tmp();
  const bin = fileURLToPath(new URL('../../../node_modules/.bin/typescript-language-server', import.meta.url));
  await mkdir(agentDir, { recursive: true });
  const server = { id: 'ts', include: ['**/*.ts'], rootMarkers: ['tsconfig.json'], bin, args: ['--stdio'], cwd: '{root}', languageIdByExtension: { '.ts': 'typescript' }, startupTimeoutMs: 20_000 };
  await writeFile(join(agentDir, 'lsp.json'), JSON.stringify({ version: 1, servers: [server] }));
  process.env.PI_AGENT_DIR = agentDir;

  let result = '';
  const faux = fauxProvider({ models: [{ id: 'w' }] });
  faux.setResponses([
    fauxAssistantMessage(fauxToolCall('checkout_pr', { repository: 'acme/widgets', number: 7 }), { stopReason: 'toolUse' }),
    (c) => { result = JSON.stringify(c.messages.at(-1)); return fauxAssistantMessage('done'); },
  ]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const extensions = [defineExtension({ name: 'gh', tools: [checkoutTool({ serviceUrl: service.url, serviceToken: 'svc', gitBaseUrl: gitBase })] }), await piPackage('pi-lsp', { from: import.meta.url })];
  const session = await openSession({ dir: await tmp(), models, agent: { model: { provider: m.provider, modelId: m.id }, extensions } });
  await session.submit('Review acme/widgets#7', 'blast-1', true);
  await session.close();

  assert.match(result, /Blast radius of changed symbols/);
  assert.match(result, /src\/math\.ts:1 function add: 2 references \(src\/app\.ts:1, src\/app\.ts:3\); tests: test\/math\.test\.ts:1, test\/math\.test\.ts:3/);
});

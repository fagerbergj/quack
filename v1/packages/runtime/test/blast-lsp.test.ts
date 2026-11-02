import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createModels, fauxAssistantMessage, fauxProvider, fauxToolCall } from '@earendil-works/pi-ai';
import { defineExtension } from '@earendil-works/pi-durable';
import { checkoutTool } from '@quack/github';
import { piPackage } from '@quack/sdk';
import { openSession } from '@quack/session';
import { cleanup, fakeService, remotes, tmp } from '../../github/test/fixtures.ts';
import { holdUntilLoaded } from '../src/extensions/lsp-hold.ts';

after(cleanup);

const base = {
  'package.json': '{ "name": "widgets", "type": "module" }\n',
  'tsconfig.json': '{ "compilerOptions": { "strict": true, "module": "nodenext" }, "include": ["src", "test"] }\n',
  'src/math.ts': 'export function add(a: number, b: number) {\n  return a + b;\n}\n',
  'src/app.ts': "import { add } from './math.ts';\n\nexport const total = add(1, 2);\n",
  'test/math.test.ts': "import { add } from '../src/math.ts';\n\nif (add(2, 2) !== 4) throw new Error('add');\n",
};

test('checkout_pr reports who calls each changed symbol, from a real language server', async () => {
  const { base: gitBase, head } = await remotes(base, { 'src/math.ts': 'export function add(a: number, b: number) {\n  return b + a;\n}\n' });
  const service = await fakeService(head);
  const agentDir = await tmp();
  const bin = fileURLToPath(new URL('../../../node_modules/.bin/typescript-language-server', import.meta.url));
  await mkdir(agentDir, { recursive: true });
  const server = { id: 'ts', include: ['**/*.ts'], rootMarkers: ['tsconfig.json'], bin, args: ['--stdio'], cwd: '{root}', languageIdByExtension: { '.ts': 'typescript' }, startupTimeoutMs: 20_000 };
  await writeFile(join(agentDir, 'lsp.json'), JSON.stringify({ version: 1, servers: [holdUntilLoaded(server)] }));
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

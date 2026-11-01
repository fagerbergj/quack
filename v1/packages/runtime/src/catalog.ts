import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { githubSessionExtensions, type GithubSessionConfig } from '@quack/github';
import { researchSessionExtensions, type ResearchConfig } from '@quack/research';
import { piPackage } from '@quack/sdk';
import { INGEST_WORKFLOW, ingestFromEnv } from '@quack/remarkable';
import { DIGEST_WORKFLOW, digestFromEnv, FINDER_WORKFLOW, finderFromEnv, LINEUP_WORKFLOW, lineupFromEnv, RETRO_WORKFLOW, retroFromEnv, TRADE_WORKFLOW, tradeFromEnv, TRENDS_WORKFLOW, trendsFromEnv, HISTORY_WORKFLOW, historyFromEnv, WAIVERS_WORKFLOW, waiversFromEnv } from '@quack/sleeper';
import type { Catalog, CatalogEntry } from '@quack/session';
import { workflowSessionExtensions, type Workflow } from '@quack/workflows';

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required by the github extension`);
  return value;
}

// pi-lsp reads its servers from $PI_AGENT_DIR/lsp.json; a reviewed repo's own .pi/lsp.json is refused without a UI to trust it.
function lspConfig(config: unknown) {
  const dir = mkdtempSync(join(tmpdir(), 'quack-pi-'));
  writeFileSync(join(dir, 'lsp.json'), JSON.stringify({ version: 1, servers: (config as { servers?: unknown[] })?.servers ?? [] }));
  process.env.PI_AGENT_DIR = dir;
  process.once('exit', () => rmSync(dir, { recursive: true, force: true }));
}

// Workflows built from the environment, by the name an agent definition binds; one entry per job as they land.
const WORKFLOWS: Record<string, (config: unknown) => Workflow> = {
  [LINEUP_WORKFLOW]: () => lineupFromEnv(),
  [WAIVERS_WORKFLOW]: () => waiversFromEnv(),
  [FINDER_WORKFLOW]: () => finderFromEnv(),
  [TRADE_WORKFLOW]: () => tradeFromEnv(),
  [TRENDS_WORKFLOW]: () => trendsFromEnv(),
  [HISTORY_WORKFLOW]: () => historyFromEnv(),
  [DIGEST_WORKFLOW]: () => digestFromEnv(),
  [RETRO_WORKFLOW]: () => retroFromEnv(),
  [INGEST_WORKFLOW]: (config) => ingestFromEnv(config as Parameters<typeof ingestFromEnv>[0]),
};

// What a workflow reads from the session process; a job's agent receives only its own.
const SLEEPER_ENV = ['QUACK_SLEEPER_URL', 'QUACK_SLEEPER_TOKEN', 'QUACK_EXA_API_KEY'];
const WORKFLOW_ENV: Record<string, string[]> = {
  ...Object.fromEntries([LINEUP_WORKFLOW, WAIVERS_WORKFLOW, FINDER_WORKFLOW, TRADE_WORKFLOW, TRENDS_WORKFLOW, HISTORY_WORKFLOW, DIGEST_WORKFLOW, RETRO_WORKFLOW].map((w) => [w, SLEEPER_ENV])),
  [INGEST_WORKFLOW]: ['QUACK_REMARKABLE_URL', 'QUACK_REMARKABLE_TOKEN'],
};

const withEnv = (env: CatalogEntry['env'], entry: CatalogEntry): CatalogEntry => Object.assign(entry, { env });

// Extensions this runtime image carries, by the name agent definitions select them with (A8).
export const catalog: Catalog = {
  coding: () => [CodingTools],
  // The service secret comes from the environment, never the definition file (D56).
  github: withEnv(['QUACK_GH_SERVICE_URL', 'QUACK_GH_SERVICE_TOKEN'], (config) => githubSessionExtensions({ ...(config as Partial<GithubSessionConfig>), serviceUrl: required('QUACK_GH_SERVICE_URL'), serviceToken: required('QUACK_GH_SERVICE_TOKEN') })),
  research: withEnv(['QUACK_EXA_API_KEY'], (config) => researchSessionExtensions(config as ResearchConfig)),
  // An agent selecting this binds its submissions to the named workflow (WFL-1).
  workflows: withEnv((config) => WORKFLOW_ENV[(config as { workflow?: string })?.workflow ?? ''] ?? [], (config) => {
    const name = (config as { workflow?: string })?.workflow ?? '';
    return workflowSessionExtensions(config, name in WORKFLOWS ? { [name]: WORKFLOWS[name](config) } : undefined);
  }),
  lsp: async (config) => {
    lspConfig(config);
    return [await piPackage('pi-lsp', { from: import.meta.url, replaySafe: ['lsp_diagnostics', 'lsp_hover', 'lsp_definition', 'lsp_references', 'lsp_symbols'] })];
  },
};

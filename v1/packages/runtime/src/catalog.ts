import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { CodingTools } from '@earendil-works/pi-durable/tools';
import { githubSessionExtensions, type GithubSessionConfig } from '@quack/github';
import { researchSessionExtensions, type ResearchConfig } from '@quack/research';
import { piPackage } from '@quack/sdk';
import { INGEST_WORKFLOW, ingestFromEnv } from '@quack/remarkable';
import { LINEUP_WORKFLOW, lineupFromEnv } from '@quack/sleeper';
import type { Catalog } from '@quack/session';
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
  [INGEST_WORKFLOW]: (config) => ingestFromEnv(config as Parameters<typeof ingestFromEnv>[0]),
};

// Extensions this runtime image carries, by the name agent definitions select them with (A8).
export const catalog: Catalog = {
  coding: () => [CodingTools],
  // The service secret comes from the environment, never the definition file (D56).
  github: (config) => githubSessionExtensions({ ...(config as Partial<GithubSessionConfig>), serviceUrl: required('QUACK_GH_SERVICE_URL'), serviceToken: required('QUACK_GH_SERVICE_TOKEN') }),
  research: (config) => researchSessionExtensions(config as ResearchConfig),
  // An agent selecting this binds its submissions to the named workflow (WFL-1).
  workflows: (config) => {
    const name = (config as { workflow?: string })?.workflow ?? '';
    return workflowSessionExtensions(config, name in WORKFLOWS ? { [name]: WORKFLOWS[name](config) } : undefined);
  },
  lsp: async (config) => {
    lspConfig(config);
    return [await piPackage('pi-lsp', { from: import.meta.url, replaySafe: ['lsp_diagnostics', 'lsp_hover', 'lsp_definition', 'lsp_references', 'lsp_symbols'] })];
  },
};

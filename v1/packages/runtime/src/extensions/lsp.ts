import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { piPackage, type ExtensionManifest } from '@quack/sdk';

// pi-lsp reads its servers from $PI_AGENT_DIR/lsp.json; a reviewed repo's own .pi/lsp.json is refused without a UI to trust it.
function lspConfig(config: unknown) {
  const dir = mkdtempSync(join(tmpdir(), 'quack-pi-'));
  writeFileSync(join(dir, 'lsp.json'), JSON.stringify({ version: 1, servers: (config as { servers?: unknown[] })?.servers ?? [] }));
  process.env.PI_AGENT_DIR = dir;
  process.once('exit', () => rmSync(dir, { recursive: true, force: true }));
}

export const manifest: ExtensionManifest = {
  name: 'lsp',
  session: async (config) => {
    lspConfig(config);
    return [await piPackage('pi-lsp', { from: import.meta.url, replaySafe: ['lsp_diagnostics', 'lsp_hover', 'lsp_definition', 'lsp_references', 'lsp_symbols'] })];
  },
};

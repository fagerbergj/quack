import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { defineConfig, type Plugin } from 'vitest/config';

// Detector D sets FLAKE_MUTANT_FILE (a source path) and FLAKE_MUTANT_SRC (its mutated text) for one run.
const target = process.env.FLAKE_MUTANT_FILE && resolve(process.env.FLAKE_MUTANT_FILE);

export const mutant: Plugin = {
  name: 'flake-spike-mutant',
  enforce: 'pre',
  load: (id) => (target && id === target ? readFileSync(process.env.FLAKE_MUTANT_SRC!, 'utf8') : undefined),
};

export default defineConfig({
  plugins: [mutant],
  // The fs module cache keys on the file on disk, so it would serve a mutant to the next run, or the original to a mutant run.
  test: { root: resolve(import.meta.dirname, '../..'), include: ['tools/flake-spike/fixtures/**/*.test.ts'], environment: 'node', fsModuleCache: false },
});
